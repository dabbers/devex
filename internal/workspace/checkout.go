package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// GitTokenEnv is the environment variable a repo's access token is expected
// under. It is an ordinary repo secret, so it is set per repo by the user and
// injected into the machine like any other.
const GitTokenEnv = "GIT_TOKEN"

// checkoutTimeout bounds a single git invocation. Cloning a large repository
// over a slow link is slow, and a timeout that trips on a first clone would
// leave a half-populated workspace that the next start would refuse to clone
// into.
const checkoutTimeout = 20 * time.Minute

// ensureCheckout makes the machine hold the repository at the workspace's
// branch.
//
// It is idempotent and deliberately non-destructive. The machine's disk
// survives a stop, and what is on it is the work: uncommitted changes, an
// in-progress rebase, a node_modules that took ten minutes to install. So a
// workspace that already has a checkout is fetched and switched, never reset,
// re-cloned or cleaned. Nothing here discards anything.
func (m *Manager) ensureCheckout(ctx context.Context, w *domain.Workspace, repo *domain.Repo, instance *vm.Instance, env map[string]string) error {
	if repo.RemoteURL == "" {
		return fmt.Errorf("workspace: repository %s has no remote to clone", repo.Name)
	}

	cloned, err := m.hasCheckout(ctx, instance, env)
	if err != nil {
		return err
	}
	if !cloned {
		// "." so the repository lands at the workspace root rather than in a
		// directory named after it, which is what every path in the service
		// commands assumes.
		if _, err := m.git(ctx, instance, env, "clone", repo.RemoteURL, "."); err != nil {
			return fmt.Errorf("workspace: clone %s: %w", repo.Name, err)
		}
		m.opts.Logger.Info("cloned the repository",
			"workspace", w.ID, "repo", repo.Name, "instance", instance.ID)
	}

	return m.switchBranch(ctx, w, repo, instance, env)
}

// hasCheckout reports whether the workspace already holds a repository.
func (m *Manager) hasCheckout(ctx context.Context, instance *vm.Instance, env map[string]string) (bool, error) {
	res, err := m.git(ctx, instance, env, "rev-parse", "--git-dir")
	if err != nil {
		return false, err
	}
	// A non-zero exit here means "not a repository", which is the ordinary
	// first-start case and not a failure.
	return res.OK(), nil
}

// switchBranch puts the checkout on the workspace's branch, creating it when
// the remote does not have it.
func (m *Manager) switchBranch(ctx context.Context, w *domain.Workspace, repo *domain.Repo, instance *vm.Instance, env map[string]string) error {
	if w.Branch == "" {
		return nil
	}

	current, err := m.git(ctx, instance, env, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if current.OK() && strings.TrimSpace(current.Stdout) == w.Branch {
		return nil
	}

	// Fetch first so a branch that exists on the remote is found even on a
	// checkout that predates it.
	if _, err := m.git(ctx, instance, env, "fetch", "--prune", "origin"); err != nil {
		return err
	}

	// Already present locally: switch to it and leave it where it is. Pulling
	// here would merge or rebase without being asked, on a branch that may
	// have uncommitted work on it.
	if res, err := m.git(ctx, instance, env, "checkout", w.Branch); err != nil {
		return err
	} else if res.OK() {
		return nil
	}

	// On the remote but not here: track it.
	if res, err := m.git(ctx, instance, env, "checkout", "-b", w.Branch, "--track", "origin/"+w.Branch); err != nil {
		return err
	} else if res.OK() {
		return nil
	}

	// Nowhere yet, so this is a new branch. Start it from the repository's
	// default rather than from whatever happened to be checked out.
	base := repo.DefaultBranch
	if base == "" {
		base = "main"
	}
	res, err := m.git(ctx, instance, env, "checkout", "-b", w.Branch, "origin/"+base)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("workspace: could not put %s on branch %s: %s",
			repo.Name, w.Branch, firstLine(res.Stderr))
	}
	m.opts.Logger.Info("started a new branch",
		"workspace", w.ID, "branch", w.Branch, "from", base)
	return nil
}

// git runs one git command in the machine's workspace.
//
// A token, when the repo has one, is handed over through a credential helper
// that reads it from the environment at the moment git asks. It therefore
// never appears in the argv -- which anyone on the machine can read out of the
// process list -- and is never written into .git/config, which would leave it
// on the disk long after the run.
func (m *Manager) git(ctx context.Context, instance *vm.Instance, env map[string]string, args ...string) (*vm.ExecResult, error) {
	argv := []string{"git"}
	if _, ok := env[GitTokenEnv]; ok {
		argv = append(argv,
			"-c", "credential.helper=",
			"-c", `credential.helper=!f() { echo username=x-access-token; echo password=$`+GitTokenEnv+`; }; f`)
	}
	argv = append(argv, args...)

	// Never prompt. Without this a missing credential leaves git waiting on a
	// terminal nobody is attached to until the timeout trips, instead of
	// failing immediately in a way that can be reported.
	gitEnv := make(map[string]string, len(env)+2)
	for k, v := range env {
		gitEnv[k] = v
	}
	gitEnv["GIT_TERMINAL_PROMPT"] = "0"
	gitEnv["GCM_INTERACTIVE"] = "never"

	res, err := m.driver.Exec(ctx, instance.ID, vm.Command{
		Argv:    argv,
		Env:     gitEnv,
		Timeout: checkoutTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: git %s: %w", strings.Join(args, " "), err)
	}
	return res, nil
}

// firstLine reduces git's output to something that fits in an error.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	if line == "" {
		return "no output"
	}
	return line
}

// gitEnv returns the environment git itself runs with.
//
// Deliberately not the machine's environment. The repo's secrets are injected
// into the instance and so are readable by the agent and by every service a
// project starts; a person's GitHub token is not a repo secret and has no
// business being there. It is handed to the git invocations that need it and
// nowhere else, so a workspace can clone a private repository without the
// credential that opened it being left lying around inside.
//
// A token the user set as a repo secret still wins: that is an explicit
// choice, and it is also what lets a repo be cloned by a machine account
// rather than by whoever happened to open the workspace.
func (m *Manager) gitEnv(ctx context.Context, w *domain.Workspace, env map[string]string) map[string]string {
	if m.gitTokens == nil {
		return env
	}
	if existing, ok := env[GitTokenEnv]; ok && existing != "" {
		return env
	}

	token, err := m.gitTokens.Token(ctx, w.UserID)
	if err != nil || token == "" {
		// Not a failure: a public repository clones without a credential, and
		// a private one fails later with git's own message, which says more
		// about what went wrong than anything that could be guessed here.
		if err != nil {
			m.opts.Logger.Info("no per-user git credential for this workspace",
				"workspace", w.ID, "user", w.UserID, "reason", err)
		}
		return env
	}

	// A copy, so the caller's map -- which does reach the machine -- is not
	// quietly given a credential.
	scoped := make(map[string]string, len(env)+1)
	for name, value := range env {
		scoped[name] = value
	}
	scoped[GitTokenEnv] = token
	return scoped
}
