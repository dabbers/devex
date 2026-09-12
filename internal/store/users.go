package store

import (
	"context"
	"errors"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

// CreateUser inserts a user, assigning an id and creation time when unset.
func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	if u.ID == "" {
		u.ID = id.New(id.User)
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO users (id, email, created_at) VALUES (?, ?, ?)",
		u.ID, u.Email, formatTime(u.CreatedAt),
	)
	return wrapErr("create user", err)
}

// GetUser looks a user up by id.
func (s *Store) GetUser(ctx context.Context, userID string) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx, "SELECT id, email, created_at FROM users WHERE id = ?", userID)
	return scanUser(row)
}

// GetUserByEmail looks a user up by email address.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := s.db.QueryRowContext(ctx, "SELECT id, email, created_at FROM users WHERE email = ?", email)
	return scanUser(row)
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
		u       domain.User
		created string
	)
	if err := row.Scan(&u.ID, &u.Email, &created); err != nil {
		return nil, wrapErr("get user", err)
	}
	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &u, nil
}
