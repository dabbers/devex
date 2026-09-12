package store

import (
	"context"
	"time"

	"github.com/dabbers/devex/internal/id"
)

// SecretRow is an encrypted secret as persisted. The control plane never
// stores plaintext: sealing and opening happen in the secrets package, which
// owns the key.
type SecretRow struct {
	ID         string
	RepoID     string
	Name       string
	Nonce      []byte
	Ciphertext []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PutSecret writes a sealed secret, replacing any existing value with the same
// name in the same repo.
func (s *Store) PutSecret(ctx context.Context, row *SecretRow) error {
	if row.ID == "" {
		row.ID = id.New(id.Secret)
	}
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (id, repo_id, name, nonce, ciphertext, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, name) DO UPDATE SET
		     nonce = excluded.nonce,
		     ciphertext = excluded.ciphertext,
		     updated_at = excluded.updated_at`,
		row.ID, row.RepoID, row.Name, row.Nonce, row.Ciphertext,
		formatTime(row.CreatedAt), formatTime(row.UpdatedAt),
	)
	return wrapErr("put secret", err)
}

// GetSecret returns one sealed secret by repo and name.
func (s *Store) GetSecret(ctx context.Context, repoID, name string) (*SecretRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo_id, name, nonce, ciphertext, created_at, updated_at
		 FROM secrets WHERE repo_id = ? AND name = ?`, repoID, name)
	return scanSecret(row)
}

// ListSecrets returns every sealed secret for a repo, ordered by name.
func (s *Store) ListSecrets(ctx context.Context, repoID string) ([]*SecretRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, name, nonce, ciphertext, created_at, updated_at
		 FROM secrets WHERE repo_id = ? ORDER BY name`, repoID)
	if err != nil {
		return nil, wrapErr("list secrets", err)
	}
	defer rows.Close()

	var secrets []*SecretRow
	for rows.Next() {
		row, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, row)
	}
	return secrets, wrapErr("list secrets", rows.Err())
}

// DeleteSecret removes a secret from a repo's scope.
func (s *Store) DeleteSecret(ctx context.Context, repoID, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE repo_id = ? AND name = ?", repoID, name)
	if err != nil {
		return wrapErr("delete secret", err)
	}
	return affectedOne("delete secret", res)
}

func scanSecret(row rowScanner) (*SecretRow, error) {
	var (
		r                SecretRow
		created, updated string
	)
	if err := row.Scan(&r.ID, &r.RepoID, &r.Name, &r.Nonce, &r.Ciphertext, &created, &updated); err != nil {
		return nil, wrapErr("get secret", err)
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
