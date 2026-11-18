package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
	"github.com/dabbers/devex/internal/store"
)

// Changing a running installation.
//
// This is the loop the product is selling: somebody asks for something, sees
// it running somewhere that is not their live site, and decides whether it
// goes live. Four rules hold it together, and each one is enforced here rather
// than asked of whoever is making the change:
//
//   - A change never touches production. It works on a branch, in its own
//     workspace, against its own copy of the data.
//   - The copy is real data, restored from a snapshot, because a change tested
//     against an empty database has not been tested.
//   - Checks come from the app's pinned upstream version, not from the
//     installation's tree. A gate the change can edit is theatre.
//   - A person promotes. Not the agent, not the checks passing.

// changeBranch names the branch a change works on.
func changeBranch(changeID string) string { return "change/" + changeID }

// OpenChange starts a change against a running installation.
func (i *Installer) OpenChange(ctx context.Context, installation *domain.Installation, title string) (*domain.Change, error) {
	if !installation.Serving() {
		return nil, fmt.Errorf("install: %s is %s; start it before changing it",
			installation.Name, installation.State)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("install: say what the change is for; it is what you will see in the history")
	}

	// One at a time. Two changes in flight against one installation means two
	// previews, two scratch databases, and a promote that has to reason about
	// what the other one did -- which is a merge conflict presented to
	// somebody who does not know what git is.
	existing, err := i.deps.Store.OpenChangeFor(ctx, installation.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("%w: %s already has a change open (%q); finish or abandon it first",
			store.ErrConflict, installation.Name, existing.Title)
	}

	// The id is minted here rather than by the store, because the branch is
	// named for it and the row has to carry both from the start.
	change := &domain.Change{
		ID:             id.New(id.Change),
		InstallationID: installation.ID,
		UserID:         installation.UserID,
		Title:          title,
		State:          domain.ChangeOpen,
	}
	change.Branch = changeBranch(change.ID)
	if err := i.deps.Store.CreateChange(ctx, change); err != nil {
		return nil, err
	}

	if err := i.prepare(ctx, installation, change); err != nil {
		return change, i.blocked(ctx, change, err)
	}
	return change, nil
}

// prepare gives a change its own copy of everything the installation has.
func (i *Installer) prepare(ctx context.Context, installation *domain.Installation, change *domain.Change) error {
	repo, err := i.deps.Store.GetRepo(ctx, installation.RepoID)
	if err != nil {
		return err
	}

	// A copy of the data, not a pointer at it. An agent experimenting cannot
	// destroy a customer's submissions because what it can reach is not the
	// customer's submissions -- and a change tested against an empty database
	// has not been tested.
	if installation.Database != "" && i.deps.Databases != nil {
		scratch, err := i.copyData(ctx, installation, change)
		if err != nil {
			return err
		}
		change.Database = scratch.Name
		if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
			return err
		}
	}

	ws := &domain.Workspace{
		UserID:    installation.UserID,
		RepoID:    repo.ID,
		ProjectID: installation.ProjectID,
		Name:      installation.Name + "-" + strings.TrimPrefix(change.ID, "chg_"),
		Branch:    change.Branch,
		State:     domain.WorkspaceStopped,
		// Scratch space a change made, so cleanup can tell it apart from
		// somewhere a person works.
		Ephemeral: true,
	}
	if err := i.deps.Store.CreateWorkspace(ctx, ws); err != nil {
		return err
	}
	change.WorkspaceID = ws.ID
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return err
	}

	started, err := i.deps.Workspaces.Start(ctx, ws)
	if err != nil {
		return err
	}
	change.PreviewURL = started.Previews.Primary

	revision, err := i.deps.Workspaces.Revision(ctx, ws)
	if err != nil {
		return err
	}
	change.Revision = revision
	return i.deps.Store.UpdateChange(ctx, change)
}

// copyData gives a change its own database, holding the installation's data as
// it was a moment ago.
//
// A fork rather than a pointer. An agent experimenting cannot destroy a
// customer's submissions because what it can reach is not the customer's
// submissions -- and a change tested against an empty database has not been
// tested, so it has to be a copy of the real thing rather than a blank one.
func (i *Installer) copyData(ctx context.Context, installation *domain.Installation, change *domain.Change) (*database.Database, error) {
	live := &database.Database{
		Name: installation.Database, Role: database.RoleFor(installation.Database),
		Owner: installation.ID,
	}
	return i.deps.Databases.Fork(ctx, live, change.ID)
}

// Promote makes a checked change live.
func (i *Installer) Promote(ctx context.Context, change *domain.Change) (*domain.Installation, error) {
	if !change.Promotable() {
		return nil, fmt.Errorf("%w: this change is %s; it goes live once its checks have passed",
			store.ErrConflict, change.State)
	}

	installation, err := i.deps.Store.GetInstallation(ctx, change.InstallationID)
	if err != nil {
		return nil, err
	}
	serving, err := i.deps.Store.GetWorkspace(ctx, installation.WorkspaceID)
	if err != nil {
		return nil, err
	}
	working, err := i.deps.Store.GetWorkspace(ctx, change.WorkspaceID)
	if err != nil {
		return nil, err
	}

	change.State = domain.ChangePromoting
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return nil, err
	}

	// Before anything is deployed. Code and data roll back differently, and
	// the snapshot is what makes the second one possible at all.
	if installation.Database != "" && i.deps.Databases != nil {
		live := &database.Database{
			Name: installation.Database, Role: database.RoleFor(installation.Database),
			Owner: installation.ID,
		}
		if _, err := i.deps.Databases.Snapshot(ctx, live, "before promoting "+change.ID); err != nil {
			return nil, i.blocked(ctx, change, fmt.Errorf("install: snapshot before promoting: %w", err))
		}
	}

	// The branch the change was worked on becomes the branch the installation
	// serves. Fast-forward only: anything else means something moved while
	// this change was open, which is a decision rather than a deploy.
	if err := i.deps.Workspaces.Publish(ctx, working, serving.Branch); err != nil {
		return nil, i.blocked(ctx, change, err)
	}
	if err := i.deps.Workspaces.Deploy(ctx, serving, change.Revision); err != nil {
		return nil, i.blocked(ctx, change, err)
	}

	now := time.Now().UTC()
	installation.PreviousRevision = installation.Revision
	installation.Revision = change.Revision
	if err := i.deps.Store.UpdateInstallation(ctx, installation); err != nil {
		return nil, err
	}

	change.State = domain.ChangePromoted
	change.StateReason = ""
	change.PromotedAt = &now
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return nil, err
	}

	// The change's own machine and scratch data are finished with. Failing to
	// tidy up is logged rather than returned: the change is live, and telling
	// the customer it failed because a scratch database lingered would be
	// wrong.
	i.release(ctx, change)
	i.deps.Logger.Info("promoted",
		"installation", installation.ID, "change", change.ID, "revision", change.Revision)
	return installation, nil
}

// Rollback returns an installation to the revision it was serving before the
// last promote.
//
// Code only. The data is not touched, because restoring a snapshot discards
// everything written since it was taken -- every form submission, every
// booking -- and that is a second, louder decision rather than part of this
// one.
func (i *Installer) Rollback(ctx context.Context, installation *domain.Installation) (*domain.Installation, error) {
	if installation.PreviousRevision == "" {
		return nil, fmt.Errorf("%w: %s has nothing to roll back to", store.ErrConflict, installation.Name)
	}
	serving, err := i.deps.Store.GetWorkspace(ctx, installation.WorkspaceID)
	if err != nil {
		return nil, err
	}

	target := installation.PreviousRevision
	if err := i.deps.Workspaces.Deploy(ctx, serving, target); err != nil {
		return nil, err
	}

	// The revisions swap rather than the previous one being dropped, so
	// rolling back is itself undoable by rolling back again.
	installation.PreviousRevision, installation.Revision = installation.Revision, target
	if err := i.deps.Store.UpdateInstallation(ctx, installation); err != nil {
		return nil, err
	}
	i.deps.Logger.Info("rolled back",
		"installation", installation.ID, "revision", target)
	return installation, nil
}

// Abandon drops a change without promoting it.
func (i *Installer) Abandon(ctx context.Context, change *domain.Change) error {
	change.State = domain.ChangeAbandoned
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return err
	}
	i.release(ctx, change)
	return nil
}

// release gives back what a change was using.
func (i *Installer) release(ctx context.Context, change *domain.Change) {
	if change.WorkspaceID != "" {
		if ws, err := i.deps.Store.GetWorkspace(ctx, change.WorkspaceID); err == nil {
			if err := i.deps.Workspaces.Stop(ctx, ws); err != nil {
				i.deps.Logger.Error("could not stop a finished change's workspace",
					"change", change.ID, "error", err)
			}
		}
	}
	if change.Database != "" && i.deps.Databases != nil {
		scratch := &database.Database{
			Name: change.Database, Role: database.RoleFor(change.Database), Owner: change.ID,
		}
		if err := i.deps.Databases.Drop(ctx, scratch); err != nil {
			i.deps.Logger.Error("could not drop a finished change's data",
				"change", change.ID, "database", change.Database, "error", err)
		}
	}
}

// blocked records why a change stopped and returns the reason.
func (i *Installer) blocked(ctx context.Context, change *domain.Change, cause error) error {
	change.State = domain.ChangeBlocked
	change.StateReason = cause.Error()
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		i.deps.Logger.Error("could not record a blocked change", "change", change.ID, "error", err)
	}
	return cause
}
