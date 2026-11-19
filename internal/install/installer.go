package install

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/appspec"
	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/workspace"
)

// Databases is the data block, as the installer needs it.
//
// An interface so the installer can be exercised without a Postgres cluster,
// and so a deployment with no data block still installs apps that do not ask
// for one.
type Databases interface {
	Provision(ctx context.Context, owner string) (*database.Database, string, error)
	Credentials(ctx context.Context, db *database.Database) (string, error)
	// Fork returns a copy of a database. How the copy is made is the block's
	// business: a dump and restore today, a copy-on-write clone when the data
	// is big enough for that to matter.
	Fork(ctx context.Context, source *database.Database, owner string) (*database.Database, error)
	Snapshot(ctx context.Context, db *database.Database, label string) (*database.Snapshot, error)
	Drop(ctx context.Context, db *database.Database) error
}

// Deps are what an installer needs.
type Deps struct {
	Store      *store.Store
	Catalog    *Catalog
	Workspaces *workspace.Manager
	Vault      *secrets.Vault
	Databases  Databases
	// Verifier drives a real browser at a change's preview. Optional: without
	// it a change is judged by its own suite alone, which notices a broken
	// query and not a blank page.
	Verifier *verify.Verifier
	// Passes lets the verifier see a preview as the change's owner. Optional:
	// without it the browser looks as a stranger, which for an app whose
	// front page is behind sign-in means a check that cannot pass.
	Passes Passes
	Logger *slog.Logger
}

// Passes mints preview passes. It is previewpass.Issuer, narrowed.
type Passes interface {
	Mint(host string, user *domain.User, ttl time.Duration) (string, error)
}

// Options configure where an installer puts things.
type Options struct {
	// RepoDir holds the platform-hosted repositories, one per installation.
	RepoDir string
	// Provided is what the platform's blocks offer, apart from the database,
	// which is provisioned per installation.
	Provided Provided
	// RepoOwner, when set, is the account workspaces run as, which is the
	// account that has to own the repositories: workspaces clone from them
	// and promote by pushing to them, and git refuses to serve a repository
	// to an account that does not own it.
	RepoOwner *Owner
}

// Owner is a Unix account, by number.
type Owner struct {
	UID, GID int
}

// Installer turns a catalog entry into something a customer is using.
type Installer struct {
	deps Deps
	opts Options
}

// New returns an installer.
func New(deps Deps, opts Options) (*Installer, error) {
	switch {
	case deps.Store == nil:
		return nil, fmt.Errorf("install: a store is required")
	case deps.Catalog == nil:
		return nil, fmt.Errorf("install: a catalog is required")
	case opts.RepoDir == "":
		return nil, fmt.Errorf("install: a directory for repositories is required")
	// An installation's services are told their database through the
	// workspace manager's per-workspace environment. A manager without it
	// starts them with no connection string, which is not a startup failure
	// here but an app exiting seconds later with a message about an unset
	// variable -- a long way from the cause. So it is a cause-side error.
	case deps.Databases != nil && deps.Workspaces != nil && !deps.Workspaces.HasOverrides():
		return nil, fmt.Errorf("install: there is a data block but the workspace manager has no per-workspace environment, " +
			"so an installation's services would start without a connection string; " +
			"pass install.NewData(store, databases) as workspace.Deps.Overrides")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	// Repositories seeded before an owner was configured belong to whoever
	// ran the control plane then. Handed over at startup, so the first sign
	// of the change is not a workspace failing to clone.
	if opts.RepoOwner != nil {
		if err := os.MkdirAll(opts.RepoDir, 0o750); err != nil {
			return nil, fmt.Errorf("install: %w", err)
		}
		if err := os.Chown(opts.RepoDir, 0, opts.RepoOwner.GID); err != nil {
			return nil, fmt.Errorf("install: give %s to the workspace group: %w", opts.RepoDir, err)
		}
		// The group has to be able to enter it, and MkdirAll leaves an
		// existing directory's mode alone. On dbz-alpha it was 0700: the
		// repositories inside were the account's, and the account could not
		// reach them, so the first promote failed with "does not appear to
		// be a git repository".
		if err := os.Chmod(opts.RepoDir, 0o750); err != nil {
			return nil, fmt.Errorf("install: %w", err)
		}
		entries, err := os.ReadDir(opts.RepoDir)
		if err != nil {
			return nil, fmt.Errorf("install: %w", err)
		}
		for _, entry := range entries {
			if err := chownTree(filepath.Join(opts.RepoDir, entry.Name()), opts.RepoOwner); err != nil {
				return nil, err
			}
		}
	}
	return &Installer{deps: deps, opts: opts}, nil
}

// chownTree hands a repository to its owner. Links are changed rather than
// followed, so a link inside cannot hand over anything outside.
func chownTree(root string, to *Owner) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, to.UID, to.GID); err != nil {
			return fmt.Errorf("install: hand %s to the workspace account: %w", path, err)
		}
		return nil
	})
}

// Request is one install.
type Request struct {
	UserID string
	App    string
	Name   string
}

// Install creates an installation and brings it up.
//
// The order is forced by what depends on what, and each step is recorded
// before the next is attempted so a failure halfway leaves something that can
// be looked at and cleaned up rather than an orphan on the cluster.
func (i *Installer) Install(ctx context.Context, req Request) (*domain.Installation, error) {
	app, err := i.deps.Catalog.Get(req.App)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = app.Spec.Name
	}

	// Refuse before provisioning anything, rather than after. An installation
	// that cannot be satisfied should leave no database behind.
	if _, missing := Environment(app.Spec, i.opts.Provided); len(missing) > 0 {
		// The database is the one block provisioned per installation, so it
		// is not missing yet and is excluded from the check here.
		if blocking := withoutDatabase(missing); len(blocking) > 0 {
			return nil, Refuse(app.Spec, blocking)
		}
	}

	// Checked before anything is created. The unique index would catch it a
	// few steps later, by which point a repository exists on disk that
	// nothing points at, and the error names a constraint rather than the
	// thing the customer did.
	existing, err := i.deps.Store.ListInstallations(ctx, store.InstallationFilter{UserID: req.UserID})
	if err != nil {
		return nil, err
	}
	for _, one := range existing {
		if strings.EqualFold(one.Name, name) {
			return nil, fmt.Errorf("%w: you already have an installation called %q",
				store.ErrConflict, name)
		}
	}

	installation := &domain.Installation{
		UserID:     req.UserID,
		App:        app.Spec.App,
		AppVersion: app.Spec.Version,
		Name:       name,
		State:      domain.InstallationProvisioning,
	}

	repo, project, err := i.record(ctx, app, installation)
	if err != nil {
		return nil, err
	}
	installation.RepoID, installation.ProjectID = repo.ID, project.ID

	if err := i.deps.Store.CreateInstallation(ctx, installation); err != nil {
		return nil, err
	}

	if err := i.provision(ctx, app, installation, repo); err != nil {
		i.fail(ctx, installation, err)
		return installation, err
	}
	return installation, nil
}

// record creates the repository and the project rows an installation runs out
// of, before anything is provisioned for it.
func (i *Installer) record(ctx context.Context, app App, installation *domain.Installation) (*domain.Repo, *domain.Project, error) {
	repo := &domain.Repo{
		UserID: installation.UserID,
		// Named for the installation rather than the app, because one account
		// may install an app more than once and each copy is its own
		// repository with its own changes.
		Name:          installation.Name,
		DefaultBranch: "main",
		Discovered:    true,
	}
	if err := i.deps.Store.CreateRepo(ctx, repo); err != nil {
		return nil, nil, err
	}

	// The repository is seeded after the row exists, so its directory can be
	// named for an id that is already recorded and an interrupted install
	// leaves nothing anonymous on disk.
	bare := filepath.Join(i.opts.RepoDir, repo.ID+".git")
	if err := seedRepository(ctx, app.Dir, bare, app.Spec.Version); err != nil {
		return nil, nil, err
	}
	if i.opts.RepoOwner != nil {
		if err := chownTree(bare, i.opts.RepoOwner); err != nil {
			return nil, nil, err
		}
	}
	repo.RemoteURL = bare
	if err := i.deps.Store.UpdateRepo(ctx, repo); err != nil {
		return nil, nil, err
	}

	project := &domain.Project{
		RepoID:       repo.ID,
		Name:         app.Spec.App,
		Path:         ".",
		BuildCommand: app.Spec.Build.Command,
		// Declared by the app's author rather than guessed by a discovery
		// pass, so there is nothing for anyone to confirm.
		Confirmed: true,
	}
	if err := i.deps.Store.CreateProject(ctx, project); err != nil {
		return nil, nil, err
	}
	for _, service := range app.Spec.DomainServices(project.ID) {
		if err := i.deps.Store.CreateService(ctx, service); err != nil {
			return nil, nil, err
		}
	}
	return repo, project, nil
}

// provision gives the installation what its manifest asked for and starts it.
func (i *Installer) provision(ctx context.Context, app App, installation *domain.Installation, repo *domain.Repo) error {
	provided := i.opts.Provided

	if app.Spec.Requires.Database.Engine != "" {
		if i.deps.Databases == nil {
			return fmt.Errorf("install: %s needs a database and this deployment has no data block",
				app.Spec.App)
		}
		db, dsn, err := i.deps.Databases.Provision(ctx, installation.ID)
		if err != nil {
			return err
		}
		installation.Database = db.Name
		// Satisfies the manifest's requirement for the check below. It is
		// deliberately not sealed with the rest: the workspace mints a fresh
		// one when it starts, so no connection string is ever stored.
		provided.DatabaseURL = dsn
		i.deps.Logger.Info("database provisioned",
			"installation", installation.ID, "database", db.Name)
	}

	env, missing := Environment(app.Spec, provided)
	if len(missing) > 0 {
		return Refuse(app.Spec, missing)
	}

	// Sealed rather than passed around. Everything an app is started with is a
	// credential or close enough to one, and the vault is already the thing
	// that injects a repo's secrets into its machine.
	if i.deps.Vault != nil {
		for name, value := range env {
			if name == "DATABASE_URL" {
				continue
			}
			if err := i.deps.Vault.Set(ctx, repo.ID, name, value); err != nil {
				return fmt.Errorf("install: seal %s: %w", name, err)
			}
		}
	}

	if err := i.deps.Store.UpdateInstallation(ctx, installation); err != nil {
		return err
	}
	return i.start(ctx, installation, repo)
}

// start brings an installation's environment up.
func (i *Installer) start(ctx context.Context, installation *domain.Installation, repo *domain.Repo) error {
	if i.deps.Workspaces == nil {
		return fmt.Errorf("install: this deployment cannot start workspaces")
	}

	ws := &domain.Workspace{
		UserID:    installation.UserID,
		RepoID:    repo.ID,
		ProjectID: installation.ProjectID,
		Name:      installation.Name,
		Branch:    repo.DefaultBranch,
		State:     domain.WorkspaceStopped,
	}
	if err := i.deps.Store.CreateWorkspace(ctx, ws); err != nil {
		return err
	}
	installation.WorkspaceID = ws.ID
	if err := i.deps.Store.UpdateInstallation(ctx, installation); err != nil {
		return err
	}

	started, err := i.deps.Workspaces.Start(ctx, ws)
	if err != nil {
		return err
	}

	// What is actually deployed. Recorded from the machine rather than
	// assumed, and recorded at install rather than at the first promote:
	// without it the first change has nothing to roll back to, which is
	// exactly when a customer is most likely to want one.
	revision, err := i.deps.Workspaces.Revision(ctx, ws)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	installation.State = domain.InstallationRunning
	installation.StateReason = ""
	installation.StartedAt = &now
	installation.URL = started.Previews.Primary
	installation.Revision = revision
	return i.deps.Store.UpdateInstallation(ctx, installation)
}

// fail records why an installation did not come up.
//
// Recorded rather than only returned, because the customer is looking at a
// page that says "provisioning" and the honest thing to put there is the
// reason it stopped.
func (i *Installer) fail(ctx context.Context, installation *domain.Installation, cause error) {
	installation.State = domain.InstallationFailed
	installation.StateReason = cause.Error()
	if err := i.deps.Store.UpdateInstallation(ctx, installation); err != nil {
		i.deps.Logger.Error("could not record a failed install",
			"installation", installation.ID, "error", err)
	}
	i.deps.Logger.Error("install failed", "installation", installation.ID, "error", cause)

	// The record, repository and database stay, so the failure can be looked
	// at. The machine does not: an install that never came up has nothing on
	// it but a checkout, and kept, it holds a full workspace's share of the
	// host. Two failed installs on dbz-alpha left no room to open a change.
	if installation.WorkspaceID == "" || i.deps.Workspaces == nil {
		return
	}
	ws, err := i.deps.Store.GetWorkspace(ctx, installation.WorkspaceID)
	if err != nil {
		i.deps.Logger.Error("could not load a failed install's workspace",
			"installation", installation.ID, "error", err)
		return
	}
	if err := i.deps.Workspaces.Release(ctx, ws); err != nil {
		i.deps.Logger.Error("could not release a failed install's machine",
			"installation", installation.ID, "error", err)
	}
}

// Delete removes an installation that never came up, and everything made for
// it, so the name is free and installing again is the retry.
//
// Only a failed one. An installation that ran has held a customer's data, and
// deleting that is a different operation with a different bar -- an export
// first, a confirmation the customer makes -- that does not exist yet. A failed
// one has nothing in its database but what the app put there on first start.
//
// Every step is safe to repeat and the records go last, so a delete that stops
// partway leaves the installation listed, still failed, and deleting it again
// finishes the job rather than leaving a database or a repository that nothing
// points at.
func (i *Installer) Delete(ctx context.Context, installation *domain.Installation) error {
	if installation.RepoID == "" {
		return fmt.Errorf("install: %s has no repository recorded", installation.ID)
	}
	if installation.State != domain.InstallationFailed {
		return fmt.Errorf("%w: %s is %s; only an installation that failed to come up can be deleted",
			store.ErrConflict, installation.Name, installation.State)
	}

	if installation.WorkspaceID != "" && i.deps.Workspaces != nil {
		ws, err := i.deps.Store.GetWorkspace(ctx, installation.WorkspaceID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			if err := i.deps.Workspaces.Release(ctx, ws); err != nil {
				return err
			}
		}
	}

	if installation.Database != "" {
		if i.deps.Databases == nil {
			return fmt.Errorf("install: %s has a database and this deployment has no data block to drop it with",
				installation.Name)
		}
		db := &database.Database{
			Name: installation.Database, Role: database.RoleFor(installation.Database),
			Owner: installation.ID,
		}
		if err := i.deps.Databases.Drop(ctx, db); err != nil {
			return err
		}
	}

	// Named for the id rather than read from the row, so a path in the
	// database can never point this at something else to remove.
	if err := os.RemoveAll(filepath.Join(i.opts.RepoDir, installation.RepoID+".git")); err != nil {
		return fmt.Errorf("install: remove repository: %w", err)
	}

	if err := i.deps.Store.DeleteRepo(ctx, installation.RepoID); err != nil {
		return err
	}
	i.deps.Logger.Info("failed installation deleted",
		"installation", installation.ID, "name", installation.Name)
	return nil
}

// withoutDatabase drops the database requirement from a list of unmet ones,
// because it is provisioned per installation rather than offered up front.
func withoutDatabase(missing []Missing) []Missing {
	kept := make([]Missing, 0, len(missing))
	for _, one := range missing {
		if one.Requirement == "database" {
			continue
		}
		kept = append(kept, one)
	}
	return kept
}

// Catalog returns what can be installed.
func (i *Installer) Catalog() *Catalog { return i.deps.Catalog }

// SpecOf returns the manifest an installation was created from.
func (i *Installer) SpecOf(installation *domain.Installation) (*appspec.Spec, error) {
	app, err := i.deps.Catalog.Get(installation.App)
	if err != nil {
		return nil, err
	}
	return app.Spec, nil
}
