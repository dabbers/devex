package install

import (
	"context"
	"errors"
	"fmt"

	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// For implements workspace.Overrides: the environment belonging to one
// workspace rather than to its repository.
//
// There is exactly one thing here and it is the database. A workspace is
// either serving an installation, in which case it gets that installation's
// data, or working on a change, in which case it gets the change's own copy --
// and the two must never be confused, because the whole safety story is that
// experimenting cannot touch anything real.
//
// The connection string is minted here rather than looked up. Nothing stores a
// DSN: not the vault, not a row, not a log. The platform owns the role, so a
// fresh credential costs an ALTER ROLE, and a credential that is never written
// down cannot be read out of storage.
func (i *Installer) For(ctx context.Context, w *domain.Workspace) (map[string]string, error) {
	name, err := i.databaseFor(ctx, w)
	if err != nil || name == "" {
		return nil, err
	}
	if i.deps.Databases == nil {
		return nil, fmt.Errorf("install: workspace %s needs database %s and there is no data block", w.ID, name)
	}

	dsn, err := i.deps.Databases.Credentials(ctx, &database.Database{
		Name: name, Role: database.RoleFor(name),
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"DATABASE_URL": dsn}, nil
}

// databaseFor reports which database a workspace should be pointed at, or ""
// if it is neither an installation nor a change.
func (i *Installer) databaseFor(ctx context.Context, w *domain.Workspace) (string, error) {
	change, err := i.deps.Store.ChangeByWorkspace(ctx, w.ID)
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

	installation, err := i.deps.Store.InstallationByWorkspace(ctx, w.ID)
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
