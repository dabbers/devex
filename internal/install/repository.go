package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Platform-hosted repositories.
//
// An installation is a fork of an app, and every mechanism in the contract --
// a change is a commit, a preview is a branch, a rollback is a previous
// revision -- is git underneath. But requiring a customer to have a GitHub
// account to run a booking page is the friction this product exists to
// remove, so the platform holds the repository and GitHub is optional.
//
// What that needs is bare repositories, not a forge. Issues, pull requests and
// code review all have better surfaces here already: the change is a preview
// with checks against it, and the review is a person looking at the running
// thing and pressing promote.

// seedTimeout bounds creating a repository.
const seedTimeout = 2 * time.Minute

// commitIdentity is who the platform commits as when it seeds a repository.
// Named rather than anonymous so the history says plainly that nobody typed
// this.
const (
	commitName  = "dabberz"
	commitEmail = "platform@dabberz.invalid"
)

// seedRepository creates a bare repository holding an app's source at one
// version, and returns the path to clone from.
//
// The installation's history starts here rather than carrying the app's own.
// A customer's repository is theirs: its first commit is the version they
// installed, and everything after it is what they changed. Importing upstream
// history would make "what have I changed" unanswerable.
func seedRepository(ctx context.Context, sourceDir, bareDir, version string) error {
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(bareDir), 0o700); err != nil {
		return fmt.Errorf("install: repository directory: %w", err)
	}
	if _, err := os.Stat(bareDir); err == nil {
		return fmt.Errorf("install: a repository already exists at %s", bareDir)
	}

	work, err := os.MkdirTemp("", "dabberz-seed-")
	if err != nil {
		return fmt.Errorf("install: staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	tree := filepath.Join(work, "tree")
	if err := copyTree(sourceDir, tree); err != nil {
		return err
	}

	// A fixed branch name rather than whatever this machine's git is
	// configured to default to, because the workspace that checks it out
	// later has to know what to ask for.
	steps := [][]string{
		{"init", "--initial-branch=main", tree},
		{"-C", tree, "add", "--all"},
		{"-C", tree,
			"-c", "user.name=" + commitName,
			"-c", "user.email=" + commitEmail,
			"commit", "--message=" + version},
		{"clone", "--bare", tree, bareDir},
	}
	for _, args := range steps {
		if err := git(ctx, args...); err != nil {
			_ = os.RemoveAll(bareDir)
			return err
		}
	}
	return nil
}

// git runs one command, reporting what it said when it fails.
func git(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	// A repository the platform creates must not inherit a developer's global
	// git configuration, which could set hooks, a signing key or a template
	// directory that would run on every clone.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0")

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install: git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// copyTree copies an app's source into a staging directory.
func copyTree(from, to string) error {
	if err := os.MkdirAll(to, 0o700); err != nil {
		return fmt.Errorf("install: staging tree: %w", err)
	}
	if err := os.CopyFS(to, os.DirFS(from)); err != nil {
		return fmt.Errorf("install: copy %s: %w", from, err)
	}
	// An app's source directory may carry a repository of its own; the
	// installation's history starts fresh.
	return os.RemoveAll(filepath.Join(to, ".git"))
}
