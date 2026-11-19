package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const installationColumns = `id, user_id, app, app_version, name, state, state_reason,
	repo_id, project_id, workspace_id, database, url, domain, revision, previous_revision,
	created_at, updated_at, started_at, access`

// encodeAccess stores a policy. The zero policy is stored as empty, which is
// what an installation made before policies existed has, and reads back as
// public; see migration 0016.
func encodeAccess(a domain.Access) (string, error) {
	if a.Mode == "" {
		return "", nil
	}
	if err := a.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(a)
	return string(raw), err
}

// decodeAccess reads a policy back. Unreadable is an error rather than a
// default: guessing public would open an internal app, and guessing internal
// would shut a public one, and neither should happen silently.
func decodeAccess(raw string) (domain.Access, error) {
	if raw == "" {
		return domain.Access{Mode: domain.AccessPublic}, nil
	}
	var a domain.Access
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return domain.Access{}, fmt.Errorf("store: unreadable access policy: %w", err)
	}
	return a, nil
}

// CreateInstallation records a new installation.
func (s *Store) CreateInstallation(ctx context.Context, i *domain.Installation) error {
	if err := i.Validate(); err != nil {
		return err
	}
	access, err := encodeAccess(i.Access)
	if err != nil {
		return err
	}
	if i.ID == "" {
		i.ID = id.New(id.Installation)
	}
	if i.State == "" {
		i.State = domain.InstallationProvisioning
	}
	now := time.Now().UTC()
	if i.CreatedAt.IsZero() {
		i.CreatedAt = now
	}
	i.UpdatedAt = now

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO installations (`+installationColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.UserID, i.App, i.AppVersion, i.Name, string(i.State), i.StateReason,
		i.RepoID, i.ProjectID, nullableString(i.WorkspaceID), i.Database, i.URL, i.Domain,
		i.Revision, i.PreviousRevision,
		formatTime(i.CreatedAt), formatTime(i.UpdatedAt), nullTime(i.StartedAt), access)
	if err != nil {
		if strings.Contains(err.Error(), "installations_name_per_user") {
			return fmt.Errorf("%w: you already have an installation called %q", ErrConflict, i.Name)
		}
		return wrapErr("create installation", err)
	}
	return nil
}

// GetInstallation loads one installation.
func (s *Store) GetInstallation(ctx context.Context, installationID string) (*domain.Installation, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+installationColumns+" FROM installations WHERE id = ?", installationID)
	return scanInstallation(row)
}

// InstallationFilter narrows a listing.
type InstallationFilter struct {
	UserID string
	App    string
	Limit  int
}

// ListInstallations returns an account's installations, newest first.
func (s *Store) ListInstallations(ctx context.Context, filter InstallationFilter) ([]*domain.Installation, error) {
	query := "SELECT " + installationColumns + " FROM installations WHERE 1 = 1"
	var args []any
	if filter.UserID != "" {
		query += " AND user_id = ?"
		args = append(args, filter.UserID)
	}
	if filter.App != "" {
		query += " AND app = ?"
		args = append(args, filter.App)
	}
	query += " ORDER BY created_at DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("list installations", err)
	}
	defer rows.Close()

	installations := make([]*domain.Installation, 0, 8)
	for rows.Next() {
		one, err := scanInstallation(rows)
		if err != nil {
			return nil, err
		}
		installations = append(installations, one)
	}
	return installations, rows.Err()
}

// UpdateInstallation writes an installation back.
func (s *Store) UpdateInstallation(ctx context.Context, i *domain.Installation) error {
	if err := i.Validate(); err != nil {
		return err
	}
	access, err := encodeAccess(i.Access)
	if err != nil {
		return err
	}
	i.UpdatedAt = time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `
		UPDATE installations SET app_version = ?, name = ?, state = ?, state_reason = ?,
			workspace_id = ?, database = ?, url = ?, domain = ?,
			revision = ?, previous_revision = ?, updated_at = ?, started_at = ?, access = ?
		WHERE id = ?`,
		i.AppVersion, i.Name, string(i.State), i.StateReason,
		nullableString(i.WorkspaceID), i.Database, i.URL, i.Domain,
		i.Revision, i.PreviousRevision,
		formatTime(i.UpdatedAt), nullTime(i.StartedAt), access, i.ID)
	if err != nil {
		return wrapErr("update installation", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: installation %s", ErrNotFound, i.ID)
	}
	return nil
}

// scannable is a row or rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanInstallation(row scannable) (*domain.Installation, error) {
	var (
		one         domain.Installation
		state       string
		workspaceID sql.NullString
		startedAt   sql.NullString
		createdAt   string
		updatedAt   string
		access      string
	)
	err := row.Scan(&one.ID, &one.UserID, &one.App, &one.AppVersion, &one.Name,
		&state, &one.StateReason, &one.RepoID, &one.ProjectID, &workspaceID,
		&one.Database, &one.URL, &one.Domain, &one.Revision, &one.PreviousRevision,
		&createdAt, &updatedAt, &startedAt, &access)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: installation", ErrNotFound)
	case err != nil:
		return nil, wrapErr("scan installation", err)
	}

	one.State = domain.InstallationState(state)
	one.WorkspaceID = workspaceID.String
	if one.Access, err = decodeAccess(access); err != nil {
		return nil, err
	}
	if one.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if one.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if one.StartedAt, err = scanNullTime(startedAt); err != nil {
		return nil, err
	}
	return &one, nil
}

// InstallationByWorkspace returns the installation a workspace is serving, if
// it is serving one.
func (s *Store) InstallationByWorkspace(ctx context.Context, workspaceID string) (*domain.Installation, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+installationColumns+" FROM installations WHERE workspace_id = ? LIMIT 1",
		workspaceID)
	return scanInstallation(row)
}
