package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

// eventColumns omits seq, which the database assigns.
const eventInsertColumns = "id, user_id, task_id, fork_id, type, message, data_json, created_at"

const eventSelectColumns = "seq, id, user_id, task_id, fork_id, type, message, data_json, created_at"

// AppendEvent records an event on the activity feed. Events are append-only:
// there is no update or delete path.
func (s *Store) AppendEvent(ctx context.Context, e *domain.Event) error {
	if e.ID == "" {
		e.ID = id.New(id.Event)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	data, err := marshalJSON(e.Data)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO events ("+eventInsertColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		e.ID, e.UserID, nullString(e.TaskID), nullString(e.ForkID), string(e.Type),
		e.Message, data, formatTime(e.CreatedAt),
	)
	if err != nil {
		return wrapErr("append event", err)
	}
	// Hand the caller the cursor the row was assigned, so it can be published
	// to live stream subscribers without a re-read.
	seq, err := res.LastInsertId()
	if err != nil {
		return wrapErr("append event", err)
	}
	e.Seq = seq
	return nil
}

// EventFilter narrows an event listing.
type EventFilter struct {
	UserID string
	TaskID string
	ForkID string
	// AfterSeq returns only events with a higher sequence number, which is how
	// a disconnected stream resumes exactly where it left off.
	AfterSeq int64
	Limit    int
}

// ListEvents returns events matching the filter, oldest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]*domain.Event, error) {
	var (
		where []string
		args  []any
	)
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if f.ForkID != "" {
		where = append(where, "fork_id = ?")
		args = append(args, f.ForkID)
	}
	if f.AfterSeq > 0 {
		where = append(where, "seq > ?")
		args = append(args, f.AfterSeq)
	}

	query := "SELECT " + eventSelectColumns + " FROM events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY seq ASC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("list events", err)
	}
	defer rows.Close()

	var events []*domain.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, wrapErr("list events", rows.Err())
}

func scanEvent(row rowScanner) (*domain.Event, error) {
	var (
		e              domain.Event
		taskID, forkID sql.NullString
		eventType      string
		data           sql.NullString
		created        string
	)
	if err := row.Scan(&e.Seq, &e.ID, &e.UserID, &taskID, &forkID, &eventType,
		&e.Message, &data, &created); err != nil {
		return nil, wrapErr("get event", err)
	}
	e.TaskID = taskID.String
	e.ForkID = forkID.String
	e.Type = domain.EventType(eventType)
	if err := unmarshalJSON(data, &e.Data); err != nil {
		return nil, err
	}
	var err error
	if e.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &e, nil
}

// nullString stores empty strings as SQL NULL so that partial indexes and
// IS NULL checks behave as expected on optional foreign keys.
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
