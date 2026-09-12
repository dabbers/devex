package store

import (
	"context"
	"time"
)

// PreviewRoute is a published mapping from a public hostname to the port a
// fork's dev server is reachable on.
type PreviewRoute struct {
	ForkID       string    `json:"fork_id"`
	Hostname     string    `json:"hostname"`
	UpstreamHost string    `json:"upstream_host"`
	UpstreamPort int       `json:"upstream_port"`
	CreatedAt    time.Time `json:"created_at"`
}

// PutPreviewRoute records a fork's preview route, replacing any existing one.
func (s *Store) PutPreviewRoute(ctx context.Context, r *PreviewRoute) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preview_routes (fork_id, hostname, upstream_host, upstream_port, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (fork_id) DO UPDATE SET
		     hostname = excluded.hostname,
		     upstream_host = excluded.upstream_host,
		     upstream_port = excluded.upstream_port`,
		r.ForkID, r.Hostname, r.UpstreamHost, r.UpstreamPort, formatTime(r.CreatedAt),
	)
	return wrapErr("put preview route", err)
}

// GetPreviewRoute returns a fork's published route.
func (s *Store) GetPreviewRoute(ctx context.Context, forkID string) (*PreviewRoute, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT fork_id, hostname, upstream_host, upstream_port, created_at FROM preview_routes WHERE fork_id = ?",
		forkID)
	return scanPreviewRoute(row)
}

// ListPreviewRoutes returns every published route, which is what the proxy
// configuration is regenerated from.
func (s *Store) ListPreviewRoutes(ctx context.Context) ([]*PreviewRoute, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT fork_id, hostname, upstream_host, upstream_port, created_at FROM preview_routes ORDER BY hostname")
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

// DeletePreviewRoute withdraws a fork's route. Routes outlive merges and
// abandonment, so this only runs on explicit manual cleanup.
func (s *Store) DeletePreviewRoute(ctx context.Context, forkID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM preview_routes WHERE fork_id = ?", forkID)
	return wrapErr("delete preview route", err)
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
	if err := row.Scan(&r.ForkID, &r.Hostname, &r.UpstreamHost, &r.UpstreamPort, &created); err != nil {
		return nil, wrapErr("get preview route", err)
	}
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &r, nil
}
