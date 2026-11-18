package install

import (
	"context"
	"errors"
	"fmt"

	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// Data implements workspace.Overrides: the environment belonging to one
// workspace rather than to its repository.
//
// There is exactly one thing here and it is the database. A workspace is
// either serving an installation, in which case it gets that installation's
// data, or working on a change, in which case it gets the change's own copy --
// and the two must never be confused, because the whole safety story is that
// experimenting cannot touch anything real.
//
// It deliberately does not hang off the installer. Deciding which database a
// workspace belongs to needs the store and the data block and nothing else, so
// making it its own thing means it can be built before the workspace manager
// instead of after it. That ordering is the entire point: a manager built
// without this starts an installation's services with no connection string,
// and the app -- correctly -- refuses to run.
type Data struct {
	store     *store.Store
	databases Databases
}

// NewData returns the per-workspace environment resolver. The data block may
// be nil, for a deployment that offers no database; a workspace that turns out
// to need one then fails with that said plainly rather than silently starting
// without it.
func NewData(st *store.Store, databases Databases) *Data {
	return &Data{store: st, databases: databases}
}

// For resolves the environment particular to this workspace.
//
// The connection string is minted here rather than looked up. Nothing stores a
// DSN: not the vault, not a row, not a log. The platform owns the role, so a
// fresh credential costs an ALTER ROLE, and a credential that is never written
// down cannot be read out of storage.
func (d *Data) For(ctx context.Context, w *domain.Workspace) (map[string]string, error) {
	name, err := d.databaseFor(ctx, w)
	if err != nil || name == "" {
		return nil, err
	}
	if d.databases == nil {
		return nil, fmt.Errorf("install: workspace %s needs database %s and there is no data block", w.ID, name)
	}

	dsn, err := d.databases.Credentials(ctx, &database.Database{
		Name: name, Role: database.RoleFor(name),
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"DATABASE_URL": dsn}, nil
}

// databaseFor reports which database a workspace should be pointed at, or ""
// if it is neither an installation nor a change.
func (d *Data) databaseFor(ctx context.Context, w *domain.Workspace) (string, error) {
	change, err := d.store.ChangeByWorkspace(ctx, w.ID)
	switch {
	case err == nil:
		// A change's scratch copy, always. Even if it has none yet, falling
		// back to the installation's live data would point an experiment at
		// the thing it exists to stay away from.
		if change.Database == "" {
			return "", fmt.Errorf("install: change %s has no copy of the data to work against", change.ID)
		}
		return change.Database, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}

	installation, err := d.store.InstallationByWorkspace(ctx, w.ID)
	switch {
	case err == nil:
		return installation.Database, nil
	case errors.Is(err, store.ErrNotFound):
		// An ordinary workspace, not part of an installation at all.
		return "", nil
	default:
		return "", err
	}
}
