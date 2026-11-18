package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// Deploying, which is the one place a workspace's checkout is treated as
// disposable.
//
// Everywhere else in this package is deliberately non-destructive: a workspace
// is where somebody works, the disk is the work, and nothing resets, cleans or
// discards it. A workspace that *serves* an installation is the opposite case.
// Nobody edits production by hand, so there is no uncommitted work to protect,
// and "whatever is on the disk" is exactly what must not decide what customers
// see. A serving workspace is the promoted revision or it is wrong.

// Revision reports the commit a workspace currently has checked out.
func (m *Manager) Revision(ctx context.Context, w *domain.Workspace) (string, error) {
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return "", err
	}
	res, err := m.git(ctx, instance, env, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("workspace: read revision: %s", firstLine(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Publish pushes a workspace's branch onto another ref in its origin.
//
// This is what promoting is underneath: the branch a change was worked on
// becomes the branch the installation serves. A fast-forward only, because
// anything else means something else moved the target while this change was
// open, and resolving that is a decision rather than a deploy.
func (m *Manager) Publish(ctx context.Context, w *domain.Workspace, target string) error {
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return err
	}
	res, err := m.git(ctx, instance, env, "push", "origin", "HEAD:refs/heads/"+target)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("workspace: publish to %s: %s", target, firstLine(res.Stdout+res.Stderr))
	}
	return nil
}

// Deploy makes a serving workspace exactly a revision, and restarts it there.
//
// Destructive on purpose: fetch, hard reset, rebuild, restart. See the note at
// the top of this file for why that is right here and wrong everywhere else.
func (m *Manager) Deploy(ctx context.Context, w *domain.Workspace, revision string) error {
	if strings.TrimSpace(revision) == "" {
		return fmt.Errorf("workspace: deploy needs a revision to deploy")
	}
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return err
	}

	if res, err := m.git(ctx, instance, env, "fetch", "origin", "--prune"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("workspace: fetch before deploy: %s", firstLine(res.Stderr))
	}
	if res, err := m.git(ctx, instance, env, "reset", "--hard", revision); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("workspace: deploy %s: %s", revision, firstLine(res.Stderr))
	}

	project, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return err
	}
	services, err := m.store.ListServices(ctx, w.ProjectID)
	if err != nil {
		return err
	}

	// Rebuilt before restarted, for the same reason as a first start: a
	// service command that runs a binary needs the binary to be the new one.
	if err := m.build(ctx, w, instance, project, m.gitEnv(ctx, w, env)); err != nil {
		return err
	}

	// The ports a running installation already answers on, kept across a
	// deploy so its public address does not move when it ships a change.
	ports := map[string]int{}
	routes, err := m.store.ListPreviewRoutesFor(ctx, w.ID)
	if err != nil {
		return err
	}
	for _, route := range routes {
		ports[route.ServiceID] = route.UpstreamPort
	}

	ordered, err := domain.StartOrder(services)
	if err != nil {
		return err
	}
	// startService stops what is there before starting, so this replaces
	// rather than duplicates.
	return m.startServices(ctx, w, instance, ordered, env, ports)
}

// reach returns the machine a workspace is on and the environment its commands
// run with.
func (m *Manager) reach(ctx context.Context, w *domain.Workspace) (*vm.Instance, map[string]string, error) {
	if w.InstanceID == "" {
		return nil, nil, fmt.Errorf("workspace: %s has no machine", w.ID)
	}
	instance, err := m.driver.Get(ctx, w.InstanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace: look up machine %s: %w", w.InstanceID, err)
	}
	env, err := m.workspaceEnvironment(ctx, w)
	if err != nil {
		return nil, nil, err
	}
	return instance, m.gitEnv(ctx, w, env), nil
}
