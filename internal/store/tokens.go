package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

// Authenticating something that is not a browser.
//
// See migration 0014 for why these exist and why minting one grants nothing
// that reading this file did not already grant.

// TokenPrefix marks a dabberz-issued token, so one can be told apart from a
// provider's token without trying to verify it as both. A provider's is a JWT
// and begins with its own header; this does not.
const TokenPrefix = "dbz_"

// APIToken is a token's record. The token itself is not in it: only the hash
// is stored, and the plain text exists once, in the hands of whoever asked
// for it.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// CreateToken issues a token for a user and returns it, once.
//
// The plain text is returned and never stored, so a caller that loses it has
// to issue another. That is the intended trade: a table of live credentials is
// worth stealing, and a table of hashes is not.
func (s *Store) CreateToken(ctx context.Context, userID, name string) (*APIToken, string, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, "", errors.New("store: a token needs a user")
	}
	if strings.TrimSpace(name) == "" {
		name = "unnamed"
	}

	// 256 bits. Enough that the hash can be a plain SHA-256 rather than a
	// password hash: there is no low-entropy secret here to grind at.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("store: generate token: %w", err)
	}
	plain := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	token := &APIToken{
		ID:        id.New(id.Token),
		UserID:    userID,
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO api_tokens (id, user_id, name, hash, created_at) VALUES (?, ?, ?, ?, ?)",
		token.ID, token.UserID, token.Name, hashToken(plain), formatTime(token.CreatedAt))
	if err != nil {
		return nil, "", wrapErr("create token", err)
	}
	return token, plain, nil
}

// UserByToken returns the user a presented token belongs to.
//
// Looked up by hash rather than compared one row at a time, which means the
// index does the work and no comparison is made against a secret in Go at all.
func (s *Store) UserByToken(ctx context.Context, presented string) (*domain.User, error) {
	presented = strings.TrimSpace(presented)
	if !strings.HasPrefix(presented, TokenPrefix) {
		return nil, fmt.Errorf("store: token: %w", ErrNotFound)
	}

	var tokenID, userID string
	err := s.db.QueryRowContext(ctx,
		"SELECT id, user_id FROM api_tokens WHERE hash = ?", hashToken(presented)).
		Scan(&tokenID, &userID)
	if err != nil {
		return nil, wrapErr("token", err)
	}

	// Best effort. A token that works and whose last use went unrecorded is a
	// worse outcome to cause than to tolerate.
	if _, err := s.db.ExecContext(ctx,
		"UPDATE api_tokens SET last_used_at = ? WHERE id = ?",
		formatTime(time.Now().UTC()), tokenID); err != nil {
		_ = err
	}

	return s.GetUser(ctx, userID)
}

// ListTokens returns a user's tokens, newest first. Never the tokens
// themselves, which are not stored.
func (s *Store) ListTokens(ctx context.Context, userID string) ([]*APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, user_id, name, created_at, last_used_at FROM api_tokens "+
			"WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, wrapErr("list tokens", err)
	}
	defer rows.Close()

	var tokens []*APIToken
	for rows.Next() {
		var t APIToken
		var created string
		var lastUsed *string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &created, &lastUsed); err != nil {
			return nil, wrapErr("list tokens", err)
		}
		if t.CreatedAt, err = parseTime(created); err != nil {
			return nil, wrapErr("list tokens", err)
		}
		if lastUsed != nil {
			when, err := parseTime(*lastUsed)
			if err != nil {
				return nil, wrapErr("list tokens", err)
			}
			t.LastUsedAt = &when
		}
		tokens = append(tokens, &t)
	}
	return tokens, wrapErr("list tokens", rows.Err())
}

// RevokeToken deletes one token. Scoped by user as well as id, so a token id
// learned from somewhere else cannot be used to revoke somebody else's.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM api_tokens WHERE id = ? AND user_id = ?", tokenID, userID)
	if err != nil {
		return wrapErr("revoke token", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrapErr("revoke token", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: revoke token: %w", ErrNotFound)
	}
	return nil
}

// hashToken is what is stored and what is matched against.
func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
