package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const serviceColumns = "id, project_id, name, kind, command, port, health_path, " +
	"exposed, is_primary, depends_on, confirmed, created_at, updated_at"

// CreateService inserts a service.
func (s *Store) CreateService(ctx context.Context, sv *domain.Service) error {
	if err := sv.Validate(); err != nil {
		return err
	}
	if sv.ID == "" {
		sv.ID = id.New(id.Service)
	}
	now := time.Now().UTC()
	if sv.CreatedAt.IsZero() {
		sv.CreatedAt = now
	}
	sv.UpdatedAt = now

	depends, err := encodeDependsOn(sv.DependsOn)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO services ("+serviceColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		sv.ID, sv.ProjectID, sv.Name, string(sv.Kind), sv.Command, sv.Port, sv.HealthPath,
		boolToInt(sv.Exposed), boolToInt(sv.Primary), depends,
		boolToInt(sv.Confirmed), formatTime(sv.CreatedAt), formatTime(sv.UpdatedAt),
	)
	return wrapErr("create service", err)
}

// GetService looks a service up by id.
func (s *Store) GetService(ctx context.Context, serviceID string) (*domain.Service, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+serviceColumns+" FROM services WHERE id = ?", serviceID)
	return scanService(row)
}

// ListServices returns a project's services in dependency order, so a caller
// that starts them in the order returned never starts one before something it
// needs.
func (s *Store) ListServices(ctx context.Context, projectID string) ([]*domain.Service, error) {
	services, err := s.listServicesUnordered(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ordered, err := domain.StartOrder(services)
	if err != nil {
		// Stored rows can describe a cycle only if they were written before
		// the check existed or edited by hand. Returning the error rather than
		// an arbitrary order means the operator is told, instead of watching
		// services start in a sequence that cannot work.
		return nil, fmt.Errorf("store: project %s: %w", projectID, err)
	}
	return ordered, nil
}

// listServicesUnordered returns a project's services in insertion order.
func (s *Store) listServicesUnordered(ctx context.Context, projectID string) ([]*domain.Service, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+serviceColumns+" FROM services WHERE project_id = ? ORDER BY id", projectID)
	if err != nil {
		return nil, wrapErr("list services", err)
	}
	defer rows.Close()

	var services []*domain.Service
	for rows.Next() {
		sv, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		services = append(services, sv)
	}
	return services, wrapErr("list services", rows.Err())
}

// GetServiceByName looks a service up by its name within a project.
func (s *Store) GetServiceByName(ctx context.Context, projectID, name string) (*domain.Service, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+serviceColumns+" FROM services WHERE project_id = ? AND name = ?", projectID, name)
	return scanService(row)
}

// UpsertService creates a service or updates the existing one with the same
// name in the same project. Discovery reruns as understanding of a repo
// improves, so it has to be idempotent per name.
func (s *Store) UpsertService(ctx context.Context, sv *domain.Service) error {
	existing, err := s.GetServiceByName(ctx, sv.ProjectID, sv.Name)
	switch {
	case err == nil:
		sv.ID = existing.ID
		sv.CreatedAt = existing.CreatedAt
		return s.UpdateService(ctx, sv)
	case errors.Is(err, ErrNotFound):
		return s.CreateService(ctx, sv)
	default:
		return err
	}
}

// UpdateService persists a mutated service.
func (s *Store) UpdateService(ctx context.Context, sv *domain.Service) error {
	if err := sv.Validate(); err != nil {
		return err
	}
	sv.UpdatedAt = time.Now().UTC()
	depends, err := encodeDependsOn(sv.DependsOn)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE services SET name = ?, kind = ?, command = ?, port = ?, health_path = ?,
		        exposed = ?, is_primary = ?, depends_on = ?, confirmed = ?, updated_at = ?
		 WHERE id = ?`,
		sv.Name, string(sv.Kind), sv.Command, sv.Port, sv.HealthPath,
		boolToInt(sv.Exposed), boolToInt(sv.Primary), depends,
		boolToInt(sv.Confirmed), formatTime(sv.UpdatedAt), sv.ID,
	)
	if err != nil {
		return wrapErr("update service", err)
	}
	return affectedOne("update service", res)
}

// DeleteService removes a service. Its preview routes go with it, so a
// hostname never outlives the thing it pointed at.
func (s *Store) DeleteService(ctx context.Context, serviceID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM services WHERE id = ?", serviceID)
	if err != nil {
		return wrapErr("delete service", err)
	}
	return affectedOne("delete service", res)
}

// ReplaceServices makes a project's service set exactly the one given, in one
// transaction.
//
// Discovery proposes a whole set rather than individual services, and applying
// it one row at a time would transiently leave two services marked primary --
// which the schema forbids -- or leave a service behind that the new set
// dropped. Both are avoided by replacing the set atomically.
func (s *Store) ReplaceServices(ctx context.Context, projectID string, services []*domain.Service) error {
	if _, err := domain.StartOrder(services); err != nil {
		return err
	}
	primaries := 0
	for _, sv := range services {
		if err := sv.Validate(); err != nil {
			return err
		}
		if sv.ProjectID != "" && sv.ProjectID != projectID {
			return fmt.Errorf("store: service %q belongs to project %s, not %s",
				sv.Name, sv.ProjectID, projectID)
		}
		if sv.Primary {
			primaries++
		}
	}
	if primaries > 1 {
		return fmt.Errorf("store: project %s was given %d front doors; at most one service may be primary",
			projectID, primaries)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapErr("replace services", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Keep the ids of services that survive by name, so preview routes
	// pointing at them are not cascaded away by the delete. Read inside the
	// transaction: deciding what to keep from a snapshot taken outside it
	// would let a concurrent write change the set under us.
	existing, err := scanServicesTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	keepID := make(map[string]string, len(existing))
	keepCreated := make(map[string]time.Time, len(existing))
	for _, sv := range existing {
		keepID[sv.Name] = sv.ID
		keepCreated[sv.Name] = sv.CreatedAt
	}

	wanted := make(map[string]bool, len(services))
	for _, sv := range services {
		wanted[sv.Name] = true
	}
	for _, sv := range existing {
		if !wanted[sv.Name] {
			if _, err := tx.ExecContext(ctx, "DELETE FROM services WHERE id = ?", sv.ID); err != nil {
				return wrapErr("replace services", err)
			}
		}
	}

	// Clear every primary flag first. Setting the new one while an old one is
	// still set would violate the single-front-door index mid-transaction.
	if _, err := tx.ExecContext(ctx,
		"UPDATE services SET is_primary = 0 WHERE project_id = ?", projectID); err != nil {
		return wrapErr("replace services", err)
	}

	now := time.Now().UTC()
	for _, sv := range services {
		sv.ProjectID = projectID
		sv.UpdatedAt = now
		depends, err := encodeDependsOn(sv.DependsOn)
		if err != nil {
			return err
		}
		if id0, ok := keepID[sv.Name]; ok {
			sv.ID = id0
			sv.CreatedAt = keepCreated[sv.Name]
			if _, err := tx.ExecContext(ctx,
				`UPDATE services SET kind = ?, command = ?, port = ?, health_path = ?,
				        exposed = ?, is_primary = ?, depends_on = ?, confirmed = ?, updated_at = ?
				 WHERE id = ?`,
				string(sv.Kind), sv.Command, sv.Port, sv.HealthPath,
				boolToInt(sv.Exposed), boolToInt(sv.Primary), depends,
				boolToInt(sv.Confirmed), formatTime(sv.UpdatedAt), sv.ID,
			); err != nil {
				return wrapErr("replace services", err)
			}
			continue
		}
		if sv.ID == "" {
			sv.ID = id.New(id.Service)
		}
		if sv.CreatedAt.IsZero() {
			sv.CreatedAt = now
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO services ("+serviceColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			sv.ID, projectID, sv.Name, string(sv.Kind), sv.Command, sv.Port, sv.HealthPath,
			boolToInt(sv.Exposed), boolToInt(sv.Primary), depends,
			boolToInt(sv.Confirmed), formatTime(sv.CreatedAt), formatTime(sv.UpdatedAt),
		); err != nil {
			return wrapErr("replace services", err)
		}
	}

	return wrapErr("replace services", tx.Commit())
}

// scanServicesTx reads a project's services through an open transaction.
func scanServicesTx(ctx context.Context, tx *sql.Tx, projectID string) ([]*domain.Service, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+serviceColumns+" FROM services WHERE project_id = ? ORDER BY id", projectID)
	if err != nil {
		return nil, wrapErr("list services", err)
	}
	defer rows.Close()

	var services []*domain.Service
	for rows.Next() {
		sv, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		services = append(services, sv)
	}
	return services, wrapErr("list services", rows.Err())
}

func encodeDependsOn(deps []string) (string, error) {
	if deps == nil {
		deps = []string{}
	}
	encoded, err := json.Marshal(deps)
	if err != nil {
		return "", fmt.Errorf("store: encode service dependencies: %w", err)
	}
	return string(encoded), nil
}

func scanService(row rowScanner) (*domain.Service, error) {
	var (
		sv                             domain.Service
		kind, depends                  string
		exposed, primary, confirmedInt int
		created, updated               string
	)
	if err := row.Scan(&sv.ID, &sv.ProjectID, &sv.Name, &kind, &sv.Command, &sv.Port,
		&sv.HealthPath, &exposed, &primary, &depends, &confirmedInt, &created, &updated); err != nil {
		return nil, wrapErr("get service", err)
	}
	sv.Kind = domain.ServiceKind(kind)
	sv.Exposed = exposed != 0
	sv.Primary = primary != 0
	sv.Confirmed = confirmedInt != 0
	if err := json.Unmarshal([]byte(depends), &sv.DependsOn); err != nil {
		return nil, fmt.Errorf("store: decode dependencies of service %s: %w", sv.ID, err)
	}
	var err error
	if sv.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if sv.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &sv, nil
}
