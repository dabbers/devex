package store

import (
	"context"
	"errors"
	"time"
)

const previewRouteColumns = "workspace_id, service_id, hostname, upstream_host, upstream_port, created_at"

// PreviewRoute is a published mapping from a public hostname to the port one
// service of one workspace is reachable on.
//
// A workspace has as many routes as its project has exposed services, and a
// fork's routes are its workspace's. Path-based routing was
// rejected: a dev server assumes it is mounted at the root, so absolute asset
// paths, absolute redirects, root-scoped cookies and HMR websockets all break
// behind a prefix unless every project is configured to match -- which cannot
// be done for arbitrary repositories.
type PreviewRoute struct {
	WorkspaceID  string    `json:"workspace_id"`
	ServiceID    string    `json:"service_id"`
	Hostname     string    `json:"hostname"`
	UpstreamHost string    `json:"upstream_host"`
	UpstreamPort int       `json:"upstream_port"`
	CreatedAt    time.Time `json:"created_at"`
}

// PutPreviewRoute records the route for one service of one workspace,
// replacing any existing one.
func (s *Store) PutPreviewRoute(ctx context.Context, r *PreviewRoute) error {
	if r.ServiceID == "" {
		return errors.New("store: a preview route needs a service")
	}
	if r.WorkspaceID == "" {
		return errors.New("store: a preview route needs a workspace")
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preview_routes (workspace_id, service_id, hostname, upstream_host, upstream_port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (workspace_id, service_id) DO UPDATE SET
		     hostname = excluded.hostname,
		     upstream_host = excluded.upstream_host,
		     upstream_port = excluded.upstream_port`,
		r.WorkspaceID, r.ServiceID, r.Hostname, r.UpstreamHost, r.UpstreamPort, formatTime(r.CreatedAt),
	)
	return wrapErr("put preview route", err)
}

// GetPreviewRoute returns the route published for one service of one
// workspace.
func (s *Store) GetPreviewRoute(ctx context.Context, workspaceID, serviceID string) (*PreviewRoute, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+previewRouteColumns+" FROM preview_routes WHERE workspace_id = ? AND service_id = ?",
		workspaceID, serviceID)
	return scanPreviewRoute(row)
}

// ListPreviewRoutesFor returns every route a workspace has published, which is
// what the UI shows and what the agent is told it can open.
func (s *Store) ListPreviewRoutesFor(ctx context.Context, workspaceID string) ([]*PreviewRoute, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+previewRouteColumns+" FROM preview_routes WHERE workspace_id = ? ORDER BY hostname",
		workspaceID)
	if err != nil {
		return nil, wrapErr("list preview routes", err)
	}
	defer rows.Close()

	var routes []*PreviewRoute
	for rows.Next() {
		r, err := scanPreviewRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, wrapErr("list preview routes", rows.Err())
}

// ListPreviewRoutes returns every published route, which is what the proxy
// configuration is regenerated from.
func (s *Store) ListPreviewRoutes(ctx context.Context) ([]*PreviewRoute, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+previewRouteColumns+" FROM preview_routes ORDER BY hostname")
	if err != nil {
		return nil, wrapErr("list preview routes", err)
	}
	defer rows.Close()

	var routes []*PreviewRoute
	for rows.Next() {
		r, err := scanPreviewRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, wrapErr("list preview routes", rows.Err())
}

// DeletePreviewRoutes withdraws every route a workspace published. Routes
// outlive merges and abandonment, so this only runs on explicit cleanup.
func (s *Store) DeletePreviewRoutes(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM preview_routes WHERE workspace_id = ?", workspaceID)
	return wrapErr("delete preview routes", err)
}

// UsedPreviewPorts returns the set of allocated upstream ports, so the
// allocator can pick a free one.
func (s *Store) UsedPreviewPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT upstream_port FROM preview_routes")
	if err != nil {
		return nil, wrapErr("list preview ports", err)
	}
	defer rows.Close()

	used := map[int]bool{}
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return nil, wrapErr("scan preview port", err)
		}
		used[port] = true
	}
	return used, wrapErr("list preview ports", rows.Err())
}

func scanPreviewRoute(row rowScanner) (*PreviewRoute, error) {
	var (
		r       PreviewRoute
		created string
	)
	if err := row.Scan(&r.WorkspaceID, &r.ServiceID, &r.Hostname, &r.UpstreamHost,
		&r.UpstreamPort, &created); err != nil {
		return nil, wrapErr("get preview route", err)
	}
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &r, nil
}
