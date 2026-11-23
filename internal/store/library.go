package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/id"
)

// LibraryFile is one file in an account's library. Its bytes are kept apart,
// by SHA256.
type LibraryFile struct {
	ID     string `json:"id"`
	UserID string `json:"-"`
	// Scope is "" for the account and an installation ID for that app's own.
	Scope       string    `json:"scope"`
	Path        string    `json:"path"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const libraryColumns = `id, user_id, scope, path, kind, content_type, size, sha256, created_by, created_at, updated_at`

// PutLibraryFile records a file, replacing whatever was at its path in its
// scope. The ID is kept across a replacement, so something pointing at it --
// a setting holding a logo -- follows the new version.
func (s *Store) PutLibraryFile(ctx context.Context, f *LibraryFile) error {
	now := time.Now().UTC()
	f.UpdatedAt = now
	existing, err := s.LibraryFileAt(ctx, f.UserID, f.Scope, f.Path)
	switch {
	case err == nil:
		f.ID, f.CreatedAt = existing.ID, existing.CreatedAt
		_, err = s.db.ExecContext(ctx, `
			UPDATE library_files SET kind = ?, content_type = ?, size = ?, sha256 = ?,
				created_by = ?, updated_at = ? WHERE id = ?`,
			f.Kind, f.ContentType, f.Size, f.SHA256, f.CreatedBy, formatTime(now), f.ID)
		return wrapErr("replace library file", err)
	case !errors.Is(err, ErrNotFound):
		return err
	}
	f.ID, f.CreatedAt = id.New(id.LibraryFile), now
	_, err = s.db.ExecContext(ctx, `INSERT INTO library_files (`+libraryColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.UserID, f.Scope, f.Path, f.Kind, f.ContentType, f.Size, f.SHA256,
		f.CreatedBy, formatTime(f.CreatedAt), formatTime(f.UpdatedAt))
	return wrapErr("add library file", err)
}

// LibraryFileAt loads the file at a path in a scope.
func (s *Store) LibraryFileAt(ctx context.Context, userID, scope, path string) (*LibraryFile, error) {
	return scanLibraryFile(s.db.QueryRowContext(ctx, `SELECT `+libraryColumns+`
		FROM library_files WHERE user_id = ? AND scope = ? AND path = ?`, userID, scope, path))
}

// GetLibraryFile loads one file by ID, and only if it is the account's.
func (s *Store) GetLibraryFile(ctx context.Context, userID, fileID string) (*LibraryFile, error) {
	return scanLibraryFile(s.db.QueryRowContext(ctx, `SELECT `+libraryColumns+`
		FROM library_files WHERE user_id = ? AND id = ?`, userID, fileID))
}

// LibraryFileByID loads one file whoever owns it, for the API's ownership
// gate, which then compares the owner itself.
func (s *Store) LibraryFileByID(ctx context.Context, fileID string) (*LibraryFile, error) {
	return scanLibraryFile(s.db.QueryRowContext(ctx, `SELECT `+libraryColumns+`
		FROM library_files WHERE id = ?`, fileID))
}

// ListLibraryFiles returns the files in the given scopes, by path.
func (s *Store) ListLibraryFiles(ctx context.Context, userID string, scopes ...string) ([]*LibraryFile, error) {
	var out []*LibraryFile
	for _, scope := range scopes {
		rows, err := s.db.QueryContext(ctx, `SELECT `+libraryColumns+`
			FROM library_files WHERE user_id = ? AND scope = ? ORDER BY path`, userID, scope)
		if err != nil {
			return nil, wrapErr("list library files", err)
		}
		for rows.Next() {
			f, err := scanLibraryFile(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, f)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteLibraryFile removes a file, reporting whether anything else still
// uses its bytes.
func (s *Store) DeleteLibraryFile(ctx context.Context, userID, fileID string) (sha string, stillUsed bool, err error) {
	f, err := s.GetLibraryFile(ctx, userID, fileID)
	if err != nil {
		return "", false, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM library_files WHERE id = ?`, f.ID); err != nil {
		return "", false, wrapErr("delete library file", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM library_files WHERE sha256 = ?`, f.SHA256).Scan(&n); err != nil {
		return "", false, wrapErr("count library uses", err)
	}
	return f.SHA256, n > 0, nil
}

func scanLibraryFile(row interface{ Scan(...any) error }) (*LibraryFile, error) {
	var f LibraryFile
	var created, updated string
	if err := row.Scan(&f.ID, &f.UserID, &f.Scope, &f.Path, &f.Kind, &f.ContentType,
		&f.Size, &f.SHA256, &f.CreatedBy, &created, &updated); err != nil {
		return nil, wrapErr("load library file", err)
	}
	var err error
	if f.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if f.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &f, nil
}

// SettingsValues returns what an installation's owner chose, by setting path.
// An installation nobody has configured has none, which is not an error.
func (s *Store) SettingsValues(ctx context.Context, installationID string) (map[string]any, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT vals FROM installation_settings WHERE installation_id = ?`, installationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, wrapErr("load settings", err)
	}
	values := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("store: unreadable settings for %s: %w", installationID, err)
	}
	return values, nil
}

// SetSettingsValues replaces an installation's chosen values.
func (s *Store) SetSettingsValues(ctx context.Context, installationID, by string, values map[string]any) error {
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO installation_settings (installation_id, vals, updated_by, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (installation_id) DO UPDATE SET vals = excluded.vals,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		installationID, string(raw), by, formatTime(time.Now().UTC()))
	return wrapErr("save settings", err)
}
