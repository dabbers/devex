package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

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

	ordered, err := domain.StartOrder(services)
	if err != nil {
		return err
	}

	// Stopped before the build, not after it. A build that writes over the
	// binary its own service is still running fails outright on Linux --
	// "text file busy" -- and the deploy is a restart anyway, so there is
	// nothing to protect by keeping the old one alive a few seconds longer.
	m.stopServices(ctx, instance, ordered)

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

// Run executes a command in a workspace and reports what happened.
//
// For the platform's own work in somebody's environment -- running a check
// suite, asking git a question -- rather than for anything a service does. The
// result is returned whole, including a non-zero exit, because for a check
// suite the exit code is the answer rather than an error.
func (m *Manager) Run(ctx context.Context, w *domain.Workspace, command string, extra map[string]string, timeout time.Duration) (*vm.ExecResult, error) {
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return nil, err
	}
	for name, value := range extra {
		env[name] = value
	}
	return m.driver.Exec(ctx, instance.ID, vm.Command{
		// Shell-quoted by the caller's shell, not by Go's %q: a command
		// carrying its own quotes loses them when a double-quoted string is
		// re-parsed.
		Argv:    []string{"sh", "-c", command},
		Env:     env,
		Timeout: timeout,
	})
}

// Changed reports which files a workspace's branch has altered since a
// revision.
//
// Used to notice when a change edits the thing that is supposed to be judging
// it. A gate the change can rewrite is theatre, and the honest answer here is
// not to forbid it -- a check sometimes has to change with the code -- but to
// say so loudly enough that a person decides.
func (m *Manager) Changed(ctx context.Context, w *domain.Workspace, since, path string) ([]string, error) {
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return nil, err
	}
	args := []string{"diff", "--name-only", since + "...HEAD"}
	if path != "" {
		args = append(args, "--", path)
	}
	res, err := m.git(ctx, instance, env, args...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("workspace: compare against %s: %s", since, firstLine(res.Stderr))
	}

	var changed []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			changed = append(changed, line)
		}
	}
	return changed, nil
}
