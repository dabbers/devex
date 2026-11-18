package install

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/appspec"
	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/workspace"
)

// Databases is the data block, as the installer needs it.
//
// An interface so the installer can be exercised without a Postgres cluster,
// and so a deployment with no data block still installs apps that do not ask
// for one.
type Databases interface {
	Provision(ctx context.Context, owner string) (*database.Database, string, error)
	Drop(ctx context.Context, db *database.Database) error
}

// Deps are what an installer needs.
type Deps struct {
	Store      *store.Store
	Catalog    *Catalog
	Workspaces *workspace.Manager
	Vault      *secrets.Vault
	Databases  Databases
	Logger     *slog.Logger
}

// Options configure where an installer puts things.
type Options struct {
	// RepoDir holds the platform-hosted repositories, one per installation.
	RepoDir string
	// Provided is what the platform's blocks offer, apart from the database,
	// which is provisioned per installation.
	Provided Provided
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
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Installer{deps: deps, opts: opts}, nil
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

	now := time.Now().UTC()
	installation.State = domain.InstallationRunning
	installation.StateReason = ""
	installation.StartedAt = &now
	installation.URL = started.Previews.Primary
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
