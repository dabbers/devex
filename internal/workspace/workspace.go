// Package workspace brings a development environment up and takes it down.
//
// It exists so there is exactly one path from "a repo on a branch" to "a
// machine with the services running and previews answering". A fork needs that
// path and so does a workspace somebody opened by hand; written twice they
// would drift, and the half that gets less use would be the half that breaks.
//
// What it does not own is what happens next. A fork's coding and verify loop
// lives in the pipeline, because that is a fork's lifecycle and not a
// workspace's: a workspace has no notion of finishing.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// GitTokens resolves the git credential a user's own work is done with.
//
// An interface, and optional: a deployment with no identity provider still
// starts workspaces, it just clones with whatever the repo's own secrets
// carry.
type GitTokens interface {
	Token(ctx context.Context, userID string) (string, error)
}

// Overrides supplies environment belonging to one workspace rather than to its
// repository.
//
// The vault is scoped to a repo, which is right for a repo's secrets and wrong
// for anything a particular workspace needs. A change working against its own
// copy of the data is exactly that case: same repository, same branch lineage,
// different database -- and its connection string must not reach the
// installation that change is proposed against.
type Overrides interface {
	For(ctx context.Context, w *domain.Workspace) (map[string]string, error)
}

// Deps are the collaborators a manager needs.
type Deps struct {
	Store   *store.Store
	Driver  vm.Driver
	Preview *preview.Allocator
	Vault   *secrets.Vault
	// GitTokens is consulted when the repo's own secrets carry no token.
	GitTokens GitTokens
	// Overrides supplies per-workspace environment. Optional.
	Overrides Overrides
}

// Options configure what a workspace gets.
type Options struct {
	// Resources is what each workspace machine is given.
	Resources vm.Resources
	// Image is the golden image machines boot.
	Image  string
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Manager starts and stops workspaces.
type Manager struct {
	store     *store.Store
	driver    vm.Driver
	preview   *preview.Allocator
	vault     *secrets.Vault
	gitTokens GitTokens
	overrides Overrides
	opts      Options
}

// New returns a manager.
func New(deps Deps, opts Options) (*Manager, error) {
	if deps.Store == nil || deps.Driver == nil {
		return nil, errors.New("workspace: a store and a driver are required")
	}
	return &Manager{
		store: deps.Store, driver: deps.Driver,
		preview: deps.Preview, vault: deps.Vault,
		gitTokens: deps.GitTokens,
		overrides: deps.Overrides,
		opts:      opts.withDefaults(),
	}, nil
}

// Started is what a running workspace offers.
type Started struct {
	Instance *vm.Instance
	Services []*domain.Service
	Previews agent.Previews
	// Env is the repo's secrets, which the caller passes to whatever it runs.
	Env map[string]string
}

// Start brings a workspace up and leaves it running.
//
// It is idempotent in the way that matters: a workspace whose machine is still
// there reuses it rather than booting a second one. Nothing is reclaimed
// automatically, so a duplicate machine is not a wasted moment but a stranded
// one.
func (m *Manager) Start(ctx context.Context, w *domain.Workspace) (*Started, error) {
	if w.State.Terminal() {
		return nil, fmt.Errorf("workspace: %s is %s and cannot be started", w.ID, w.State)
	}

	// Compare-and-set, so two requests to start one workspace cannot both
	// proceed. Already running is not an error: the caller wanted it up.
	if w.State != domain.WorkspaceStarting {
		moved, err := m.store.TransitionWorkspace(ctx, w.ID, w.State, domain.WorkspaceStarting,
			"bringing the machine and services up")
		if err != nil {
			if w.State == domain.WorkspaceRunning {
				return m.describe(ctx, w)
			}
			return nil, err
		}
		*w = *moved
	}

	started, err := m.bringUp(ctx, w)
	if err != nil {
		// Record why on the workspace itself. A caller that only logs would
		// leave a workspace stuck in starting with the reason in a file
		// nobody reads.
		if _, failErr := m.store.TransitionWorkspace(ctx, w.ID,
			domain.WorkspaceStarting, domain.WorkspaceFailed, truncate(err.Error(), 400)); failErr != nil {
			m.opts.Logger.Error("could not record a failed start",
				"workspace", w.ID, "error", failErr)
		}
		return nil, err
	}

	running, err := m.store.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStarting, domain.WorkspaceRunning, "services started")
	if err != nil {
		return nil, err
	}
	*w = *running
	return started, nil
}

// bringUp does the work: machine, secrets, previews.
func (m *Manager) bringUp(ctx context.Context, w *domain.Workspace) (*Started, error) {
	repo, err := m.store.GetRepo(ctx, w.RepoID)
	if err != nil {
		return nil, err
	}
	project, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	// Dependency order, so nothing starts before what it reads.
	services, err := m.store.ListServices(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}

	env, err := m.workspaceEnvironment(ctx, w)
	if err != nil {
		return nil, err
	}

	instance, err := m.machine(ctx, w, repo, env)
	if err != nil {
		return nil, err
	}
	w.InstanceID = instance.ID
	if err := m.store.UpdateWorkspace(ctx, w); err != nil {
		return nil, err
	}

	// The code has to be on the machine before anything else makes sense: an
	// agent in an empty directory and a preview route pointing at a port
	// nothing serves are both the same mistake.
	if err := m.ensureCheckout(ctx, w, repo, instance, m.gitEnv(ctx, w, env)); err != nil {
		return nil, err
	}

	// Build before services, because a service command that runs a binary
	// needs the binary to exist, and the source only arrived a moment ago.
	if err := m.build(ctx, w, instance, project, env); err != nil {
		return nil, err
	}

	// Routes are allocated before anything starts, because allocation is what
	// decides which port each service listens on: two installations of the
	// same app on one machine would otherwise both bind the port their
	// manifest declares. Allocating is not publishing -- nothing is routed to
	// until the services behind it answer.
	previews := agent.Previews{URLs: map[string]string{}}
	routes := map[string]*store.PreviewRoute{}
	ports := map[string]int{}
	var primary string
	if m.preview != nil {
		var err error
		routes, primary, err = m.preview.AllocateAll(ctx, preview.Target{ID: w.ID, Name: w.Name}, project, services, instance.Address)
		if err != nil {
			return nil, err
		}
		for serviceID, route := range routes {
			ports[serviceID] = route.UpstreamPort
		}
	}

	if err := m.startServices(ctx, w, instance, services, env, ports); err != nil {
		return nil, err
	}

	if m.preview != nil {
		// Published only now: a route to a port nothing serves is a link that
		// goes nowhere, and the whole point of a preview is that it is the
		// running thing.
		if err := m.preview.Publish(ctx); err != nil {
			return nil, err
		}
		for _, service := range services {
			if route, ok := routes[service.ID]; ok {
				previews.URLs[service.Name] = m.preview.URL(route)
			}
		}
		previews.Primary = primary
		w.PreviewURL = primary
		if err := m.store.UpdateWorkspace(ctx, w); err != nil {
			return nil, err
		}
	}

	return &Started{Instance: instance, Services: services, Previews: previews, Env: env}, nil
}

// machine returns the workspace's VM, reusing the existing one when it is
// still there.
func (m *Manager) machine(ctx context.Context, w *domain.Workspace, repo *domain.Repo, env map[string]string) (*vm.Instance, error) {
	if w.InstanceID != "" {
		instance, err := m.driver.Get(ctx, w.InstanceID)
		switch {
		case err == nil:
			// The disk is the work. Reusing it is what makes a restart
			// cheap and a workspace a place rather than a job.
			m.opts.Logger.Info("reusing the workspace machine",
				"workspace", w.ID, "instance", instance.ID)
			return instance, nil
		case errors.Is(err, vm.ErrNotFound):
			m.opts.Logger.Warn("the workspace machine is gone; booting a new one",
				"workspace", w.ID, "instance", w.InstanceID)
		default:
			return nil, fmt.Errorf("workspace: look up machine %s: %w", w.InstanceID, err)
		}
	}

	instance, err := m.driver.Create(ctx, vm.Spec{
		ForkID:    w.ID,
		Name:      w.Name,
		Resources: m.opts.Resources,
		Image:     m.opts.Image,
		Env:       env,
		Labels: map[string]string{
			"dabberz.workspace": w.ID,
			"dabberz.repo":      repo.Name,
			"dabberz.branch":    w.Branch,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: provision a machine for %s: %w", w.ID, err)
	}
	return instance, nil
}

// Stop shuts a workspace down, leaving its machine and disk in place.
func (m *Manager) Stop(ctx context.Context, w *domain.Workspace) error {
	if !w.State.Occupies() {
		return nil
	}
	moved, err := m.store.TransitionWorkspace(ctx, w.ID, w.State, domain.WorkspaceStopping,
		"shutting the services down")
	if err != nil {
		return err
	}
	*w = *moved

	// The services go down; the machine is deliberately left alone. Nothing
	// is reclaimed automatically, and the disk holds uncommitted work.
	if w.InstanceID != "" {
		if instance, err := m.driver.Get(ctx, w.InstanceID); err == nil {
			services, err := m.store.ListServices(ctx, w.ProjectID)
			if err != nil {
				return err
			}
			m.stopServices(ctx, instance, services)
		} else if !errors.Is(err, vm.ErrNotFound) {
			return fmt.Errorf("workspace: look up machine %s: %w", w.InstanceID, err)
		}
	}

	stopped, err := m.store.TransitionWorkspace(ctx, w.ID,
		domain.WorkspaceStopping, domain.WorkspaceStopped, "")
	if err != nil {
		return err
	}
	*w = *stopped
	return nil
}

// Resume brings back up every workspace recorded as running, after the
// control plane itself restarted.
//
// The services a workspace runs are children of this process as far as the
// service manager is concerned, so stopping the daemon stops them, and the
// record still says running. Until this existed every deploy and every
// reboot left each installation answering 502, and on dbz-alpha each deploy
// was followed by somebody restarting every workspace by hand -- which is the
// opposite of what is being sold.
//
// Each is stopped and started again, the same way that person did it. That
// rebuilds what is on the disk and leaves the checkout where it stands:
// Start switches branch only when it is on the wrong one, and never fetches
// over or resets a checkout, so an installation that was rolled back comes
// back on the revision it was rolled back to, and a change keeps its
// uncommitted work. One at a time, because they share the machine's ports
// and CPU; and one that fails does not stop the rest.
func (m *Manager) Resume(ctx context.Context) error {
	recorded, err := m.store.ListWorkspaces(ctx, store.WorkspaceFilter{
		// Starting too: a restart in the middle of bringing one up leaves it
		// there, and nothing else would ever finish the job.
		States:           []domain.WorkspaceState{domain.WorkspaceRunning, domain.WorkspaceStarting},
		IncludeEphemeral: true,
	})
	if err != nil {
		return fmt.Errorf("workspace: list what was running: %w", err)
	}

	var failed []string
	for _, w := range recorded {
		if err := m.resume(ctx, w); err != nil {
			m.opts.Logger.Error("a workspace did not come back after the restart",
				"workspace", w.ID, "name", w.Name, "error", err)
			failed = append(failed, w.Name)
			continue
		}
		m.opts.Logger.Info("workspace resumed", "workspace", w.ID, "name", w.Name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("workspace: %d of %d did not come back: %s",
			len(failed), len(recorded), strings.Join(failed, ", "))
	}
	return nil
}

func (m *Manager) resume(ctx context.Context, w *domain.Workspace) error {
	if w.State == domain.WorkspaceRunning {
		// Its services are gone but the record does not know it. Stopping
		// first settles the record, and tolerates there being nothing to stop.
		if err := m.Stop(ctx, w); err != nil {
			return err
		}
	}
	_, err := m.Start(ctx, w)
	return err
}

// Release stops a workspace and destroys its machine, giving back what the
// machine held.
//
// Stop keeps the machine on purpose, because a workspace's disk can hold work
// nobody has committed. That reasoning is about a workspace somebody is still
// using. A change that was promoted has its work merged, an abandoned one was
// thrown away on purpose, and an install that never came up has nothing on its
// disk but a checkout. Kept, each held its full share of the host forever:
// nothing else ever destroys a machine, so on dbz-alpha the first change used
// the last free slot and no second one could start.
func (m *Manager) Release(ctx context.Context, w *domain.Workspace) error {
	if err := m.Stop(ctx, w); err != nil {
		return err
	}
	if w.InstanceID == "" {
		return nil
	}
	// A failed start never reached Stop's service shutdown, and may have left
	// something running before it failed.
	if instance, err := m.driver.Get(ctx, w.InstanceID); err == nil {
		services, err := m.store.ListServices(ctx, w.ProjectID)
		if err != nil {
			return err
		}
		m.stopServices(ctx, instance, services)
	}
	if err := m.driver.Destroy(ctx, w.InstanceID); err != nil && !errors.Is(err, vm.ErrNotFound) {
		return fmt.Errorf("workspace: destroy machine %s: %w", w.InstanceID, err)
	}
	w.InstanceID = ""
	return m.store.UpdateWorkspace(ctx, w)
}

// describe reports an already-running workspace without changing anything.
func (m *Manager) describe(ctx context.Context, w *domain.Workspace) (*Started, error) {
	current, err := m.store.GetWorkspace(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	*w = *current
	if w.InstanceID == "" {
		return nil, fmt.Errorf("workspace: %s is running but has no machine", w.ID)
	}
	instance, err := m.driver.Get(ctx, w.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: look up machine %s: %w", w.InstanceID, err)
	}
	services, err := m.store.ListServices(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	env, err := m.workspaceEnvironment(ctx, w)
	if err != nil {
		return nil, err
	}

	previews := agent.Previews{URLs: map[string]string{}, Primary: w.PreviewURL}
	routes, err := m.store.ListPreviewRoutesFor(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*domain.Service, len(services))
	for _, service := range services {
		byID[service.ID] = service
	}
	for _, route := range routes {
		if service := byID[route.ServiceID]; service != nil && m.preview != nil {
			previews.URLs[service.Name] = m.preview.URL(route)
		}
	}
	return &Started{Instance: instance, Services: services, Previews: previews, Env: env}, nil
}

// workspaceEnvironment is the repo's secrets with this workspace's own
// environment laid over them.
//
// The override wins, because it is the more specific statement: a change's
// database is this workspace's database, whatever the repository says.
func (m *Manager) workspaceEnvironment(ctx context.Context, w *domain.Workspace) (map[string]string, error) {
	env, err := m.environment(ctx, w.RepoID)
	if err != nil {
		return nil, err
	}
	if m.overrides == nil {
		return env, nil
	}
	particular, err := m.overrides.For(ctx, w)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve environment for %s: %w", w.ID, err)
	}
	for name, value := range particular {
		env[name] = value
	}
	return env, nil
}

// HasOverrides reports whether per-workspace environment is wired.
//
// It exists so that whatever depends on that environment can refuse at
// startup rather than let a service start without it.
func (m *Manager) HasOverrides() bool { return m.overrides != nil }

// environment resolves the repo's secrets.
func (m *Manager) environment(ctx context.Context, repoID string) (map[string]string, error) {
	if m.vault == nil {
		return map[string]string{}, nil
	}
	env, err := m.vault.Environment(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve secrets for repo %s: %w", repoID, err)
	}
	return env, nil
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
