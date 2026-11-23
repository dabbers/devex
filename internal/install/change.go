package install

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
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

	// Several at once, each with its own branch, machine and copy of the
	// data -- but only as many as the host has room for. Asked before
	// anything is spent: the driver would refuse the machine anyway, but
	// only after the data had been copied, leaving a half-made change and a
	// database behind to explain.
	if err := i.deps.Workspaces.Room(ctx); err != nil {
		return nil, fmt.Errorf("install: no room for another change to %s; finish or abandon one first: %w",
			installation.Name, err)
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
		// Two changes opened at the same moment can both see room for one.
		// The driver turns the second away rather than overcommit, and that
		// change is given back whole rather than left blocked holding a copy
		// of the data: there is nothing in it worth keeping.
		if errors.Is(err, vm.ErrNoCapacity) {
			i.release(ctx, change)
			change.State, change.StateReason = domain.ChangeAbandoned, "the host had no room for it"
			if updateErr := i.deps.Store.UpdateChange(ctx, change); updateErr != nil {
				i.deps.Logger.Error("could not record a change turned away", "change", change.ID, "error", updateErr)
			}
			return nil, fmt.Errorf("install: no room for another change to %s; finish or abandon one first: %w",
				installation.Name, err)
		}
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
	i.refreshSettings(ctx, installation, ws)

	revision, err := i.deps.Workspaces.Revision(ctx, ws)
	if err != nil {
		return err
	}
	change.Revision = revision
	// Where it started. Production being anywhere else by the time it is
	// promoted means another change went live while this one was open, and
	// the panel can say so before anybody presses promote.
	change.BaseRevision = revision
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
	return i.deps.Databases.Fork(ctx, i.liveData(installation), change.ID)
}

// Promote makes a checked change live.
//
// Several changes can be open at once, and they go live in the order the
// customer promotes them: each call means "this one next". One installation's
// promotes wait for each other, so the second always starts from what the
// first left live.
//
// When production has moved on since the change started -- another change
// went live first -- this one is brought up to date before it follows, by
// merging production into its branch. If that merges cleanly, the result is
// checked again, against a fresh copy of production's data, before it goes
// live; what passed before was a different commit. If the two changes edit the
// same lines, nothing goes live: the change becomes conflicted, with the
// files recorded, for its own agent to resolve.
func (i *Installer) Promote(ctx context.Context, change *domain.Change) (*domain.Installation, error) {
	unlock := i.promotions.lock(change.InstallationID)
	defer unlock()

	// Read again under the lock. A promote that was waiting may be holding a
	// copy from before the one ahead of it changed things.
	fresh, err := i.deps.Store.GetChange(ctx, change.ID)
	if err != nil {
		return nil, err
	}
	*change = *fresh

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

	// The checks passed on a particular commit. If the branch has moved since
	// -- an agent committing once more after being told it was ready, a person
	// pushing by hand -- then what would go live is not what was checked, and
	// "nothing goes live unchecked" has to mean this or it means nothing.
	current, err := i.deps.Workspaces.Revision(ctx, working)
	if err != nil {
		return nil, err
	}
	if current != change.Revision {
		return nil, fmt.Errorf("%w: this change has been worked on since its checks passed "+
			"(checked %s, now %s); check it again before putting it live",
			store.ErrConflict, short(change.Revision), short(current))
	}

	// The same promise about edits not yet committed. What goes live is the
	// commit, so they would not -- and a merge would either refuse over them
	// or carry them into a commit nobody checked.
	uncommitted, err := i.deps.Workspaces.Uncommitted(ctx, working)
	if err != nil {
		return nil, err
	}
	if len(uncommitted) > 0 {
		return nil, fmt.Errorf("%w: this change has edits that are not committed (%s); "+
			"commit or discard them and check it again",
			store.ErrConflict, strings.Join(uncommitted, ", "))
	}

	// Another change went live while this one was open. Without this the
	// push below is refused as not a fast-forward and the change is merely
	// blocked -- or, with a push that did not insist on fast-forwards, this
	// change would go live without the one before it.
	upToDate, err := i.deps.Workspaces.Contains(ctx, working, installation.Revision)
	if err != nil {
		return nil, err
	}
	if !upToDate {
		if err := i.catchUp(ctx, installation, change, working); err != nil {
			return nil, err
		}
	}

	change.State = domain.ChangePromoting
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return nil, err
	}

	// Before anything is deployed. Code and data roll back differently, and
	// the snapshot is what makes the second one possible at all.
	if installation.Database != "" && i.deps.Databases != nil {
		if _, err := i.deps.Databases.Snapshot(ctx, i.liveData(installation), "before promoting "+change.ID); err != nil {
			return nil, i.blocked(ctx, change, fmt.Errorf("install: snapshot before promoting: %w", err))
		}
	}

	// The branch the change was worked on becomes the branch the installation
	// serves. Fast-forward only: anything else means something moved that
	// catching up did not account for, which is a decision rather than a
	// deploy.
	//
	// Code only. The change's copy of the data is never brought back: it is
	// production as it was plus whatever the agent experimented with, and
	// production has gone on taking real submissions since. The app's
	// migrations run when the serving workspace restarts on the new revision,
	// against production's own data as it stands.
	if err := i.deps.Workspaces.Publish(ctx, working, serving.Branch); err != nil {
		return nil, i.blocked(ctx, change, err)
	}
	if err := i.deps.Workspaces.Deploy(ctx, serving, change.Revision); err != nil {
		return nil, i.blocked(ctx, change, err)
	}
	// The promoted code may declare settings the old did not.
	i.refreshSettings(ctx, installation, serving)

	now := time.Now().UTC()
	installation.PreviousRevision = installation.Revision
	installation.Revision = change.Revision
	if err := i.deps.Store.UpdateInstallation(ctx, installation); err != nil {
		return nil, err
	}

	change.State = domain.ChangePromoted
	change.StateReason = ""
	change.Conflicts = nil
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

// catchUp brings a change up to date with what production now serves, and
// checks it again.
//
// A merge rather than a rebase. The change's commits are the ones its checks
// and its agent's conversation refer to; a rebase rewrites every one of them
// and replays them one by one, so a single overlap can come up as a conflict
// several times over. A merge is one commit that says "production, then this",
// with the history the agent made left as it was -- and it has production's
// own commit as a parent, so pushing the result is still a fast-forward.
func (i *Installer) catchUp(ctx context.Context, installation *domain.Installation, change *domain.Change, working *domain.Workspace) error {
	message := fmt.Sprintf("Bring %q up to date with production at %s", change.Title, short(installation.Revision))
	conflicts, err := i.deps.Workspaces.Merge(ctx, working, installation.Revision, message)
	if err != nil {
		return i.blocked(ctx, change, fmt.Errorf("install: bring the change up to date with production: %w", err))
	}
	if len(conflicts) > 0 {
		change.State = domain.ChangeConflicted
		change.Conflicts = conflicts
		change.StateReason = "production has moved on since this change started, and the two edit the same lines in " +
			strings.Join(conflicts, ", ")
		if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
			return err
		}
		i.deps.Logger.Info("change conflicts with production",
			"change", change.ID, "production", installation.Revision, "files", conflicts)
		return fmt.Errorf("%w: %s; hand it to this change's agent to resolve, then check it again",
			store.ErrConflict, change.StateReason)
	}
	change.Conflicts = nil

	// A fresh copy of production's data before checking again. The copy the
	// change was opened with is as old as the change, and production may
	// have taken rows since that the merged code's migrations have never met
	// -- which is exactly where a migration fails. The preview restarts on
	// the new copy as part of the check, so the migrations meet it there
	// first, rather than on production.
	stale, err := i.refreshData(ctx, installation, change)
	if err != nil {
		return i.blocked(ctx, change, fmt.Errorf("install: copy production's current data for the re-check: %w", err))
	}
	if stale != nil {
		defer i.dropScratch(ctx, change, stale)
	}

	checked, err := i.Check(ctx, change)
	if err != nil {
		return err
	}
	if !checked.Passed() {
		return fmt.Errorf("%w: brought up to date with production, this change no longer passes its checks; "+
			"nothing went live", store.ErrConflict)
	}
	return nil
}

// refreshData replaces a change's copy of the data with a new copy of
// production's, and returns the old one for the caller to drop once nothing is
// using it. Nil, with no error, for an installation without a database.
func (i *Installer) refreshData(ctx context.Context, installation *domain.Installation, change *domain.Change) (*database.Database, error) {
	if installation.Database == "" || i.deps.Databases == nil || change.Database == "" {
		return nil, nil
	}
	fresh, err := i.copyData(ctx, installation, change)
	if err != nil {
		return nil, err
	}
	stale := &database.Database{
		Name: change.Database, Role: database.RoleFor(change.Database), Owner: change.ID,
	}
	change.Database = fresh.Name
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return nil, err
	}
	return stale, nil
}

// ConflictBrief is what a conflicted change's own agent is told, in its own
// workspace, so that it can resolve the conflict knowing what both sides were
// for.
//
// That agent is the one to ask because it made this change and its
// conversation holds why. It runs on the customer's own subscription like any
// other turn in that workspace; the platform makes no model call of its own.
func (i *Installer) ConflictBrief(ctx context.Context, change *domain.Change) (string, error) {
	if change.State != domain.ChangeConflicted {
		return "", fmt.Errorf("%w: this change is %s; only a conflicted change has a conflict to hand over",
			store.ErrConflict, change.State)
	}
	installation, err := i.deps.Store.GetInstallation(ctx, change.InstallationID)
	if err != nil {
		return "", err
	}

	// What went live in the meantime, in the words it was asked for in, so
	// the agent knows what the other side of the conflict was trying to do.
	others, err := i.deps.Store.ListChanges(ctx, installation.ID, 100)
	if err != nil {
		return "", err
	}
	var since []string
	for _, other := range others {
		if other.ID != change.ID && other.State == domain.ChangePromoted &&
			other.PromotedAt != nil && other.PromotedAt.After(change.CreatedAt) {
			since = append(since, fmt.Sprintf("%q", other.Title))
		}
	}
	went := "another change"
	if len(since) > 0 {
		went = strings.Join(since, ", ")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Production has moved on since this change was started. While you were working on %q, "+
		"%s went live, and production is now at commit %s.\n\n", change.Title, went, installation.Revision)
	b.WriteString("Bringing this branch up to date with production conflicts in:\n")
	for _, file := range change.Conflicts {
		fmt.Fprintf(&b, "- %s\n", file)
	}
	fmt.Fprintf(&b, "\nPlease resolve it here:\n"+
		"1. Run `git fetch origin` and then `git merge %s`.\n"+
		"2. Resolve each conflict so that both changes still do what they were for: keep what production now has "+
		"and add what this change is for. Do not drop either side to make the conflict go away.\n"+
		"3. Build the app and run its tests.\n"+
		"4. Commit the merge. Do not push: the platform checks the result, and the customer decides when it goes live.\n\n"+
		"When you are done, say in a sentence or two how you combined them.", installation.Revision)
	return b.String(), nil
}

// liveData names an installation's own database.
func (i *Installer) liveData(installation *domain.Installation) *database.Database {
	return &database.Database{
		Name: installation.Database, Role: database.RoleFor(installation.Database),
		Owner: installation.ID,
	}
}

// dropScratch drops one of a change's old copies of the data, logging rather
// than failing: a lingering scratch database is untidy, not wrong.
func (i *Installer) dropScratch(ctx context.Context, change *domain.Change, scratch *database.Database) {
	if err := i.deps.Databases.Drop(ctx, scratch); err != nil {
		i.deps.Logger.Error("could not drop a change's old copy of the data",
			"change", change.ID, "database", scratch.Name, "error", err)
	}
}

// perKey is a set of mutexes, one per key, made on first use. Never shrunk:
// one per installation that has ever promoted is a few bytes each.
type perKey struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock takes the key's mutex and returns the function that gives it back.
func (p *perKey) lock(key string) func() {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = map[string]*sync.Mutex{}
	}
	m, ok := p.locks[key]
	if !ok {
		m = &sync.Mutex{}
		p.locks[key] = m
	}
	p.mu.Unlock()
	m.Lock()
	return m.Unlock
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
	current := installation.Revision

	// The branch moves with what is live. Every change starts from it, so
	// leaving it at the commit just rolled back would put that commit into
	// the next change, and approving that change would put it live again
	// without anyone having asked: on dbz-alpha a restyle opened after a
	// rollback carried the rolled-back work along with it. The commit stays
	// under a ref of its own, so rolling forward still has it.
	repo, err := i.deps.Store.GetRepo(ctx, installation.RepoID)
	if err != nil {
		return nil, err
	}
	bare := filepath.Join(i.opts.RepoDir, repo.ID+".git")
	branch := "refs/heads/" + repo.DefaultBranch
	if current != "" {
		if err := git(ctx, "-c", "safe.directory=*", "-C", bare,
			"update-ref", "refs/dabberz/rolled-back/"+current, current); err != nil {
			return nil, err
		}
	}
	if err := git(ctx, "-c", "safe.directory=*", "-C", bare, "update-ref", branch, target); err != nil {
		return nil, err
	}
	if i.opts.RepoOwner != nil {
		if err := chownTree(bare, i.opts.RepoOwner); err != nil {
			return nil, err
		}
	}

	if err := i.deps.Workspaces.Deploy(ctx, serving, target); err == nil {
		i.refreshSettings(ctx, installation, serving)
	} else {
		// Put the branch back, so it still says what is actually live.
		if current != "" {
			if undo := git(ctx, "-c", "safe.directory=*", "-C", bare, "update-ref", branch, current); undo != nil {
				i.deps.Logger.Error("could not put the branch back after a failed rollback",
					"installation", installation.ID, "error", undo)
			}
		}
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
			// Released rather than stopped: what a finished change did is
			// merged or deliberately discarded, so its machine holds nothing
			// worth the capacity it would go on claiming.
			if err := i.deps.Workspaces.Release(ctx, ws); err != nil {
				i.deps.Logger.Error("could not release a finished change's workspace",
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

// short abbreviates a revision for a message a person reads.
func short(revision string) string {
	if len(revision) > 10 {
		return revision[:10]
	}
	return revision
}
