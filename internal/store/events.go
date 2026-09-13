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
const eventInsertColumns = "id, user_id, repo_id, task_id, fork_id, actor, type, message, data_json, created_at"

const eventSelectColumns = "seq, id, user_id, repo_id, task_id, fork_id, actor, type, message, data_json, created_at"

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
		"INSERT INTO events ("+eventInsertColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.ID, e.UserID, nullString(e.RepoID), nullString(e.TaskID), nullString(e.ForkID),
		string(e.Actor), string(e.Type), e.Message, data, formatTime(e.CreatedAt),
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
//
// The zero value returns everything, which is the unified cross-repo view;
// each field narrows it without changing the shape of the result.
type EventFilter struct {
	UserID string
	// RepoID narrows the feed to one repo.
	RepoID string
	TaskID string
	ForkID string
	// Actor narrows to one actor, which is how the unattended half of the
	// trail is separated from what the user did.
	Actor domain.Actor
	// Types narrows to particular event types.
	Types []domain.EventType
	// AfterSeq returns only events with a higher sequence number, which is how
	// a disconnected stream resumes exactly where it left off.
	AfterSeq int64
	// BeforeSeq returns only events with a lower sequence number, which is how
	// a reader pages backwards through the trail.
	BeforeSeq int64
	// Newest returns the most recent events first. Combined with Limit this
	// gives the latest N rather than the earliest N, which is what a feed
	// someone is reading wants; a stream resuming from a cursor wants the
	// default oldest-first order instead.
	Newest bool
	Limit  int
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
	if f.RepoID != "" {
		where = append(where, "repo_id = ?")
		args = append(args, f.RepoID)
	}
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if f.ForkID != "" {
		where = append(where, "fork_id = ?")
		args = append(args, f.ForkID)
	}
	if f.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, string(f.Actor))
	}
	if len(f.Types) > 0 {
		where = append(where, "type IN ("+placeholders(len(f.Types))+")")
		for _, t := range f.Types {
			args = append(args, string(t))
		}
	}
	if f.AfterSeq > 0 {
		where = append(where, "seq > ?")
		args = append(args, f.AfterSeq)
	}
	if f.BeforeSeq > 0 {
		where = append(where, "seq < ?")
		args = append(args, f.BeforeSeq)
	}

	query := "SELECT " + eventSelectColumns + " FROM events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Newest {
		query += " ORDER BY seq DESC"
	} else {
		query += " ORDER BY seq ASC"
	}
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
		e                      domain.Event
		repoID, taskID, forkID sql.NullString
		actor                  string
		eventType              string
		data                   sql.NullString
		created                string
	)
	if err := row.Scan(&e.Seq, &e.ID, &e.UserID, &repoID, &taskID, &forkID,
		&actor, &eventType, &e.Message, &data, &created); err != nil {
		return nil, wrapErr("get event", err)
	}
	e.RepoID = repoID.String
	e.Actor = domain.Actor(actor)
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
