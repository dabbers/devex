package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const taskColumns = `id, user_id, repo_id, title, request, state, merge_target, merge_timing,
	integration_branch, plan_json, state_reason, created_at, updated_at`

// CreateTask inserts a task, defaulting the merge preferences when the caller
// left them unset.
func (s *Store) CreateTask(ctx context.Context, t *domain.Task) error {
	if t.ID == "" {
		t.ID = id.New(id.Task)
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.State == "" {
		t.State = domain.TaskDraft
	}
	if t.MergeTarget == "" {
		t.MergeTarget = domain.MergeTargetDefaultBranch
	}
	if t.MergeTiming == "" {
		t.MergeTiming = domain.MergeTimingImmediate
	}

	plan, err := marshalJSON(t.Plan)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO tasks ("+taskColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.ID, t.UserID, t.RepoID, t.Title, t.Request, string(t.State),
		string(t.MergeTarget), string(t.MergeTiming), t.IntegrationBranch,
		plan, t.StateReason, formatTime(t.CreatedAt), formatTime(t.UpdatedAt),
	)
	return wrapErr("create task", err)
}

// GetTask looks a task up by id.
func (s *Store) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", taskID)
	return scanTask(row)
}

// TaskFilter narrows a task listing.
type TaskFilter struct {
	UserID string
	RepoID string
	// States, when non-empty, restricts the listing to these states.
	States []domain.TaskState
	Limit  int
}

// ListTasks returns tasks matching the filter, newest first.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]*domain.Task, error) {
	var (
		where []string
		args  []any
	)
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.RepoID != "" {
		where = append(where, "repo_id = ?")
		args = append(args, f.RepoID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+placeholders(len(f.States))+")")
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}

	query := "SELECT " + taskColumns + " FROM tasks"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("list tasks", err)
	}
	defer rows.Close()

	var tasks []*domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, wrapErr("list tasks", rows.Err())
}

// UpdateTask persists a mutated task.
func (s *Store) UpdateTask(ctx context.Context, t *domain.Task) error {
	t.UpdatedAt = time.Now().UTC()
	plan, err := marshalJSON(t.Plan)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET title = ?, request = ?, state = ?, merge_target = ?, merge_timing = ?,
		        integration_branch = ?, plan_json = ?, state_reason = ?, updated_at = ?
		 WHERE id = ?`,
		t.Title, t.Request, string(t.State), string(t.MergeTarget), string(t.MergeTiming),
		t.IntegrationBranch, plan, t.StateReason, formatTime(t.UpdatedAt), t.ID,
	)
	if err != nil {
		return wrapErr("update task", err)
	}
	return affectedOne("update task", res)
}

func scanTask(row rowScanner) (*domain.Task, error) {
	var (
		t                domain.Task
		state            string
		target, timing   string
		plan             sql.NullString
		created, updated string
	)
	if err := row.Scan(&t.ID, &t.UserID, &t.RepoID, &t.Title, &t.Request, &state,
		&target, &timing, &t.IntegrationBranch, &plan, &t.StateReason,
		&created, &updated); err != nil {
		return nil, wrapErr("get task", err)
	}
	t.State = domain.TaskState(state)
	t.MergeTarget = domain.MergeTarget(target)
	t.MergeTiming = domain.MergeTiming(timing)
	if err := unmarshalJSON(plan, &t.Plan); err != nil {
		return nil, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if t.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &t, nil
}

// placeholders renders "?, ?, ?" for an IN clause of n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
