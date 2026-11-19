package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// Bringing a workspace's branch up to date with somewhere else, and the
// questions that decide whether it needs to be.
//
// Several changes can be open against one installation at once, each on its
// own branch cut from production as it was. Whichever goes live second finds
// production has moved, and has to take in what went before it rather than
// overwrite it. That is a merge, done here in the change's own workspace, so
// that the result is something the change's checks and its agent can see.

// mergeIdentity is who the platform's own merge commits are by. Not the
// agent's identity: the agent did not write this commit, and a person reading
// the history should be able to tell the two apart.
var mergeIdentity = []string{
	"-c", "user.name=dabberz",
	"-c", "user.email=platform@dabberz.invalid",
}

// Room reports whether the host can fit one more workspace machine, and if
// not, says so in terms a person can act on.
//
// Asked before anything is spent on a new workspace. The driver refuses to
// overcommit regardless, but only when the machine is created, which for a
// change is after its copy of the data has been made: a refusal then leaves a
// database and a half-made change behind to explain.
func (m *Manager) Room(ctx context.Context) error {
	capacity, err := m.driver.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("workspace: read the host's capacity: %w", err)
	}
	if capacity.CanFit(m.opts.Resources) {
		return nil
	}
	return fmt.Errorf("%w: this host has %s free and a workspace needs %s (%d running)",
		vm.ErrNoCapacity, capacity.Free(), m.opts.Resources, capacity.Instances)
}

// Contains reports whether a revision is already part of a workspace's
// branch, which is what "up to date with it" means.
func (m *Manager) Contains(ctx context.Context, w *domain.Workspace, revision string) (bool, error) {
	if strings.TrimSpace(revision) == "" {
		return true, nil
	}
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return false, err
	}
	if err := m.fetch(ctx, instance, env); err != nil {
		return false, err
	}
	res, err := m.git(ctx, instance, env, "merge-base", "--is-ancestor", revision, "HEAD")
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("workspace: compare with %s: %s", revision, firstLine(res.Stderr))
	}
}

// Uncommitted lists tracked files a workspace has edited and not committed.
//
// Untracked files are not counted: a build leaves its binary in the checkout,
// and that is not work. What matters is an edit to something the branch
// holds, which a merge could either refuse over or quietly carry along.
func (m *Manager) Uncommitted(ctx context.Context, w *domain.Workspace) ([]string, error) {
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return nil, err
	}
	res, err := m.git(ctx, instance, env, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("workspace: read status: %s", firstLine(res.Stderr))
	}
	var files []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if len(line) > 3 {
			files = append(files, strings.TrimSpace(line[3:]))
		}
	}
	return files, nil
}

// Merge takes a revision into a workspace's branch as a merge commit.
//
// When the two edit the same lines it reports which files and leaves the
// workspace exactly as it was. Not half-merged, with conflict markers in the
// tree: that is a preview that no longer builds and a branch that is neither
// the change nor its resolution, sitting there until somebody notices. The
// agent that resolves it does the merge again itself, knowing why.
func (m *Manager) Merge(ctx context.Context, w *domain.Workspace, revision, message string) (conflicts []string, err error) {
	instance, env, err := m.reach(ctx, w)
	if err != nil {
		return nil, err
	}
	if err := m.fetch(ctx, instance, env); err != nil {
		return nil, err
	}

	args := append(append([]string{}, mergeIdentity...),
		"merge", "--no-ff", "--no-edit", "-m", message, revision)
	res, err := m.git(ctx, instance, env, args...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 0 {
		return nil, nil
	}

	unmerged, listErr := m.git(ctx, instance, env, "diff", "--name-only", "--diff-filter=U")
	if listErr == nil && unmerged.ExitCode == 0 {
		for _, line := range strings.Split(unmerged.Stdout, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				conflicts = append(conflicts, line)
			}
		}
	}
	// Put it back whatever went wrong. A merge that stopped for any reason
	// leaves state behind that the next commit would silently finish.
	if abort, abortErr := m.git(ctx, instance, env, "merge", "--abort"); abortErr != nil {
		return conflicts, abortErr
	} else if abort.ExitCode != 0 && len(conflicts) > 0 {
		return conflicts, fmt.Errorf("workspace: could not undo the conflicted merge: %s", firstLine(abort.Stderr))
	}
	if len(conflicts) > 0 {
		return conflicts, nil
	}
	return nil, fmt.Errorf("workspace: merge %s: %s", revision, firstLine(res.Stdout+res.Stderr))
}

// fetch brings the workspace's view of its origin up to date.
func (m *Manager) fetch(ctx context.Context, instance *vm.Instance, env map[string]string) error {
	res, err := m.git(ctx, instance, env, "fetch", "origin", "--prune")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("workspace: fetch: %s", firstLine(res.Stderr))
	}
	return nil
}
