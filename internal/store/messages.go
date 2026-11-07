package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const messageColumns = "id, workspace_id, user_id, role, content, state, state_reason, " +
	"usage_json, created_at, updated_at"

// ErrAgentBusy means the workspace's agent is already working on something.
var ErrAgentBusy = errors.New("store: the agent is already working in this workspace")

// AppendMessage records one turn of a workspace's conversation.
func (s *Store) AppendMessage(ctx context.Context, m *domain.Message) error {
	if !m.Role.Valid() {
		return fmt.Errorf("store: %q is not a message role", m.Role)
	}
	if m.State == "" {
		m.State = domain.MessageDone
	}
	if !m.State.Valid() {
		return fmt.Errorf("store: %q is not a message state", m.State)
	}
	if m.ID == "" {
		m.ID = id.New(id.Message)
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now

	usage, err := marshalJSON(m.Usage)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO workspace_messages ("+messageColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		m.ID, m.WorkspaceID, m.UserID, string(m.Role), m.Content, string(m.State), m.StateReason,
		usage, formatTime(m.CreatedAt), formatTime(m.UpdatedAt),
	)
	return wrapErr("append message", err)
}

// StartAgentTurn records a user's message and the agent's reply-in-progress in
// one transaction, refusing if the agent is already working.
//
// Both in one step because the check and the claim have to be atomic: two
// messages sent at once would otherwise each see an idle agent, and the second
// run would start on a machine the first is already changing.
func (s *Store) StartAgentTurn(ctx context.Context, prompt *domain.Message) (*domain.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, wrapErr("start agent turn", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var running int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM workspace_messages WHERE workspace_id = ? AND state = ?",
		prompt.WorkspaceID, string(domain.MessageRunning),
	).Scan(&running); err != nil {
		return nil, wrapErr("start agent turn", err)
	}
	if running > 0 {
		return nil, ErrAgentBusy
	}

	now := time.Now().UTC()
	prompt.ID = id.New(id.Message)
	prompt.Role = domain.MessageFromUser
	prompt.State = domain.MessageDone
	prompt.CreatedAt, prompt.UpdatedAt = now, now

	reply := &domain.Message{
		ID:          id.New(id.Message),
		WorkspaceID: prompt.WorkspaceID,
		UserID:      prompt.UserID,
		Role:        domain.MessageFromAgent,
		State:       domain.MessageRunning,
		// Ordering comes from the insert sequence, not from these: both are
		// written in the same transaction and may share a millisecond.
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, m := range []*domain.Message{prompt, reply} {
		usage, err := marshalJSON(m.Usage)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO workspace_messages ("+messageColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			m.ID, m.WorkspaceID, m.UserID, string(m.Role), m.Content, string(m.State), m.StateReason,
			usage, formatTime(m.CreatedAt), formatTime(m.UpdatedAt),
		); err != nil {
			return nil, wrapErr("start agent turn", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapErr("start agent turn", err)
	}
	return reply, nil
}

// FinishMessage records how a turn ended.
func (s *Store) FinishMessage(ctx context.Context, m *domain.Message) error {
	if !m.State.Valid() {
		return fmt.Errorf("store: %q is not a message state", m.State)
	}
	m.UpdatedAt = time.Now().UTC()
	usage, err := marshalJSON(m.Usage)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspace_messages SET content = ?, state = ?, state_reason = ?,
		        usage_json = ?, updated_at = ?
		 WHERE id = ?`,
		m.Content, string(m.State), m.StateReason, usage, formatTime(m.UpdatedAt), m.ID,
	)
	if err != nil {
		return wrapErr("finish message", err)
	}
	return affectedOne("finish message", res)
}

// ListMessages returns a workspace's conversation oldest first.
func (s *Store) ListMessages(ctx context.Context, workspaceID string, limit int) ([]*domain.Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+messageColumns+" FROM workspace_messages WHERE workspace_id = ? "+
			"ORDER BY seq LIMIT ?", workspaceID, limit)
	if err != nil {
		return nil, wrapErr("list messages", err)
	}
	defer rows.Close()

	var messages []*domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, wrapErr("list messages", rows.Err())
}

// AbandonRunningMessages marks every in-flight turn failed.
//
// Called once at startup. A run lives in a goroutine, so a restart ends it
// without ending the row: without this a workspace would show an agent
// working forever and refuse every new message, with nothing to point at.
func (s *Store) AbandonRunningMessages(ctx context.Context, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE workspace_messages SET state = ?, state_reason = ?, updated_at = ? WHERE state = ?",
		string(domain.MessageFailed), reason, formatTime(time.Now().UTC()), string(domain.MessageRunning))
	if err != nil {
		return 0, wrapErr("abandon running messages", err)
	}
	return res.RowsAffected()
}

func scanMessage(row rowScanner) (*domain.Message, error) {
	var (
		m                domain.Message
		role, state      string
		usage            sql.NullString
		created, updated string
	)
	if err := row.Scan(&m.ID, &m.WorkspaceID, &m.UserID, &role, &m.Content,
		&state, &m.StateReason, &usage, &created, &updated); err != nil {
		return nil, wrapErr("get message", err)
	}
	m.Role = domain.MessageRole(role)
	m.State = domain.MessageState(state)
	if err := unmarshalJSON(usage, &m.Usage); err != nil {
		return nil, err
	}
	var err error
	if m.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if m.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &m, nil
}
