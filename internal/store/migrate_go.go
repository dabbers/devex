package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

// migrationStep applies one migration inside a transaction the runner owns.
type migrationStep func(ctx context.Context, tx *sql.Tx) error

// goMigrations holds the migrations that cannot be expressed in SQL, keyed by
// a name that sorts into the same sequence as the .sql files.
var goMigrations = map[string]migrationStep{
	"0004_backfill_services.go": backfillServices,
}

// backfillServices converts the single preview command and port each project
// used to carry into a one-element service set, re-keys preview routes by
// service, and then drops the old columns.
//
// It is written in Go because every row it inserts needs an identifier from
// internal/id -- a type prefix plus a time-sortable base32 body -- and SQLite
// can produce neither. Doing the conversion and the column drop in one
// transaction is what guarantees there is never a moment where a project
// describes how it runs in two places.
func backfillServices(ctx context.Context, tx *sql.Tx) error {
	type legacy struct {
		projectID string
		name      string
		toolchain string
		command   string
		port      int
		confirmed int
		createdAt string
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, toolchain, preview_command, preview_port, confirmed, created_at
		 FROM projects ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}
	var projects []legacy
	for rows.Next() {
		var p legacy
		if err := rows.Scan(&p.projectID, &p.name, &p.toolchain, &p.command, &p.port,
			&p.confirmed, &p.createdAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read projects: %w", err)
	}

	now := formatTime(time.Now().UTC())
	// serviceFor maps a project to the service its old preview route should be
	// re-keyed to.
	serviceFor := make(map[string]string, len(projects))

	for _, p := range projects {
		command := strings.TrimSpace(p.command)
		if command == "" && p.port == 0 {
			// The project never had a runnable preview. Inventing a service
			// with a guessed command would be worse than leaving the set
			// empty for the user or discovery to fill in.
			continue
		}

		serviceID := id.New(id.Service)
		// Everything that existed before was the project's one dev server, so
		// it becomes the web front door. A port is required to expose it; a
		// project that had a command but no port is kept as an unexposed
		// service rather than being dropped, because the command is real
		// information even when the port was never recorded.
		exposed := p.port > 0
		depends, err := json.Marshal([]string{})
		if err != nil {
			return fmt.Errorf("encode depends_on: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO services
			     (id, project_id, name, kind, command, port, health_path,
			      exposed, is_primary, depends_on, confirmed, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)`,
			serviceID, p.projectID, "web", string(domain.ServiceWeb), command, p.port,
			boolToInt(exposed), boolToInt(exposed), string(depends), p.confirmed,
			p.createdAt, now,
		); err != nil {
			return fmt.Errorf("insert service for project %s: %w", p.projectID, err)
		}
		if exposed {
			serviceFor[p.projectID] = serviceID
		}
	}

	if err := rekeyPreviewRoutes(ctx, tx, serviceFor); err != nil {
		return err
	}

	// Two sources of truth for how a project runs is the bug this whole
	// migration exists to prevent, so the old columns go in the same
	// transaction that populated their replacement.
	for _, column := range []string{"preview_command", "preview_port"} {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE projects DROP COLUMN "+column); err != nil {
			return fmt.Errorf("drop projects.%s: %w", column, err)
		}
	}
	return nil
}

// rekeyPreviewRoutes rebuilds preview_routes so a route belongs to one service
// of one fork rather than to a fork as a whole, carrying existing routes over
// to the service backfilled for their fork's project.
func rekeyPreviewRoutes(ctx context.Context, tx *sql.Tx, serviceFor map[string]string) error {
	type route struct {
		forkID, hostname, upstreamHost, createdAt string
		upstreamPort                              int
		projectID                                 string
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT r.fork_id, r.hostname, r.upstream_host, r.upstream_port, r.created_at, f.project_id
		 FROM preview_routes r JOIN forks f ON f.id = r.fork_id`)
	if err != nil {
		return fmt.Errorf("read preview routes: %w", err)
	}
	var existing []route
	for rows.Next() {
		var r route
		if err := rows.Scan(&r.forkID, &r.hostname, &r.upstreamHost, &r.upstreamPort,
			&r.createdAt, &r.projectID); err != nil {
			rows.Close()
			return fmt.Errorf("scan preview route: %w", err)
		}
		existing = append(existing, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read preview routes: %w", err)
	}

	// SQLite cannot change a primary key in place, so the table is rebuilt.
	// defer_foreign_keys keeps the drop-and-recreate from tripping the
	// constraint checks mid-transaction.
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE preview_routes"); err != nil {
		return fmt.Errorf("drop preview_routes: %w", err)
	}
	const create = `
CREATE TABLE preview_routes (
    fork_id       TEXT NOT NULL REFERENCES forks (id) ON DELETE CASCADE,
    service_id    TEXT NOT NULL REFERENCES services (id) ON DELETE CASCADE,
    hostname      TEXT NOT NULL UNIQUE,
    upstream_host TEXT NOT NULL,
    upstream_port INTEGER NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (fork_id, service_id)
) STRICT;

CREATE INDEX idx_preview_routes_port ON preview_routes (upstream_port);
CREATE INDEX idx_preview_routes_fork ON preview_routes (fork_id);`
	if _, err := tx.ExecContext(ctx, create); err != nil {
		return fmt.Errorf("recreate preview_routes: %w", err)
	}

	for _, r := range existing {
		serviceID, ok := serviceFor[r.projectID]
		if !ok {
			// The fork's project had no exposed service to inherit the route,
			// so there is nothing for it to belong to. Dropping it is correct:
			// the hostname described a dev server that is no longer
			// represented, and keeping an orphan would let the proxy publish a
			// route nothing can explain.
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO preview_routes
			     (fork_id, service_id, hostname, upstream_host, upstream_port, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.forkID, serviceID, r.hostname, r.upstreamHost, r.upstreamPort, r.createdAt,
		); err != nil {
			return fmt.Errorf("carry over preview route for fork %s: %w", r.forkID, err)
		}
	}
	return nil
}
