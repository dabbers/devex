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

const userColumns = "id, external_id, email, created_at"

// CreateUser inserts a user, assigning an id and creation time when unset.
func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	if u.ID == "" {
		u.ID = id.New(id.User)
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO users ("+userColumns+") VALUES (?, ?, ?, ?)",
		u.ID, nullableString(u.ExternalID), u.Email, formatTime(u.CreatedAt),
	)
	return wrapErr("create user", err)
}

// GetUser looks a user up by id.
func (s *Store) GetUser(ctx context.Context, userID string) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", userID)
	return scanUser(row)
}

// GetUserByEmail looks a user up by email address.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE email = ?", email)
	return scanUser(row)
}

// GetUserByExternalID looks a user up by the identity provider's subject.
func (s *Store) GetUserByExternalID(ctx context.Context, externalID string) (*domain.User, error) {
	if externalID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE external_id = ?", externalID)
	return scanUser(row)
}

// EnsureUserByExternalID resolves the dabberz user behind a provider identity,
// creating them on first sign-in.
//
// Lookup is by subject, not email, because a provider can let a person change
// their address and can reassign a freed one. Matching on email would hand a
// new account somebody else's history.
//
// There is one exception, and it is deliberate: a user that already exists
// with this email and no external id yet is adopted rather than duplicated.
// That is what carries an existing single-user installation over to a provider
// without stranding everything it has done. Once adopted the subject is what
// identifies them, so the same email arriving under a different subject later
// creates a separate user.
func (s *Store) EnsureUserByExternalID(ctx context.Context, externalID, email string) (*domain.User, error) {
	if externalID == "" {
		return nil, errors.New("store: an external identity needs a subject")
	}

	switch found, err := s.GetUserByExternalID(ctx, externalID); {
	case err == nil:
		// Keep the email current: a person can change theirs at the provider,
		// and the audit trail should name them as they are now.
		if email != "" && found.Email != email {
			found.Email = email
			if err := s.updateUserEmail(ctx, found.ID, email); err != nil {
				return nil, err
			}
		}
		return found, nil
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	if email != "" {
		switch existing, err := s.GetUserByEmail(ctx, email); {
		case err == nil && existing.ExternalID == "":
			existing.ExternalID = externalID
			if _, err := s.db.ExecContext(ctx,
				"UPDATE users SET external_id = ? WHERE id = ? AND external_id IS NULL",
				externalID, existing.ID,
			); err != nil {
				return nil, wrapErr("adopt user", err)
			}
			return existing, nil
		case err == nil:
			// Same address, different provider account. Two people, or one
			// person with two accounts; either way not the same user.
			return nil, fmt.Errorf("store: %q already belongs to another identity", email)
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}
	}

	created := &domain.User{ExternalID: externalID, Email: email}
	if err := s.CreateUser(ctx, created); err != nil {
		return nil, err
	}
	return created, nil
}

// updateUserEmail records a changed address.
func (s *Store) updateUserEmail(ctx context.Context, userID, email string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET email = ? WHERE id = ?", email, userID)
	return wrapErr("update user email", err)
}

// EnsureUser returns the user with the given email, creating them if absent.
// v1 is single-user, so the daemon calls this once at startup to resolve the
// owner it will scope everything to.
func (s *Store) EnsureUser(ctx context.Context, email string) (*domain.User, error) {
	u, err := s.GetUserByEmail(ctx, email)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	u = &domain.User{Email: email}
	if err := s.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (*domain.User, error) {
	var (
		u          domain.User
		externalID sql.NullString
		created    string
	)
	if err := row.Scan(&u.ID, &externalID, &u.Email, &created); err != nil {
		return nil, wrapErr("get user", err)
	}
	u.ExternalID = externalID.String
	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &u, nil
}

// nullableString stores an empty string as SQL NULL, so the unique index on
// external_id does not treat every provider-less user as a duplicate of the
// others.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
