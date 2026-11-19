package store

import (
	"context"
	"time"

	"github.com/dabbers/devex/internal/id"
)

// CredentialRow is a sealed per-user credential as persisted. Like SecretRow,
// the store never sees plaintext; the secrets package seals and opens it.
type CredentialRow struct {
	ID         string
	UserID     string
	Kind       string
	Nonce      []byte
	Ciphertext []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PutCredential writes a sealed credential, replacing any existing one of the
// same kind for the same user.
func (s *Store) PutCredential(ctx context.Context, row *CredentialRow) error {
	if row.ID == "" {
		row.ID = id.New(id.Secret)
	}
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_credentials (id, user_id, kind, nonce, ciphertext, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, kind) DO UPDATE SET
		     nonce = excluded.nonce,
		     ciphertext = excluded.ciphertext,
		     updated_at = excluded.updated_at`,
		row.ID, row.UserID, row.Kind, row.Nonce, row.Ciphertext,
		formatTime(row.CreatedAt), formatTime(row.UpdatedAt),
	)
	return wrapErr("put credential", err)
}

// GetCredential returns one sealed credential by user and kind.
func (s *Store) GetCredential(ctx context.Context, userID, kind string) (*CredentialRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, kind, nonce, ciphertext, created_at, updated_at
		 FROM user_credentials WHERE user_id = ? AND kind = ?`, userID, kind)

	var (
		r                CredentialRow
		created, updated string
	)
	if err := row.Scan(&r.ID, &r.UserID, &r.Kind, &r.Nonce, &r.Ciphertext, &created, &updated); err != nil {
		return nil, wrapErr("get credential", err)
	}
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if r.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteCredential removes a user's credential of one kind.
func (s *Store) DeleteCredential(ctx context.Context, userID, kind string) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM user_credentials WHERE user_id = ? AND kind = ?", userID, kind)
	if err != nil {
		return wrapErr("delete credential", err)
	}
	return affectedOne("delete credential", res)
}
