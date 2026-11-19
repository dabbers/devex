package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestARolledBackChangeDoesNotComeBackWithTheNextOne: on dbz-alpha a change
// was promoted and rolled back, and the next change opened -- a restyle --
// carried the rolled-back commit with it, because rolling back moved what was
// deployed and not the branch every change starts from. Approving the restyle
// would have put the rolled-back work live again without anyone asking.
func TestARolledBackChangeDoesNotComeBackWithTheNextOne(t *testing.T) {
	if testing.Short() {
		t.Skip("provisions a database")
	}
	dsn := os.Getenv(clusterEnv)
	if dsn == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	installer, db, owner, _, _ := newInstaller(t, dsn)
	installation := installApp(t, ctx, installer, owner.ID, "rollback")
	live := installation.Revision

	unwanted := openAndCommit(t, ctx, installer, db, installation, "Something to regret",
		`echo 'rolled back' >> README.md`)
	markReady(t, ctx, installer, db, unwanted)
	promoted, err := installer.Promote(ctx, unwanted)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	rolled, err := installer.Rollback(ctx, promoted)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolled.Revision != live {
		t.Fatalf("rolled back to %s, want %s", rolled.Revision, live)
	}

	bare := filepath.Join(installer.opts.RepoDir, installation.RepoID+".git")
	if tip := revParse(t, bare, "refs/heads/main"); tip != live {
		t.Fatalf("after the rollback main is %s; the live revision is %s", tip, live)
	}

	next, err := installer.OpenChange(ctx, rolled, "A restyle")
	if err != nil {
		t.Fatalf("OpenChange: %v", err)
	}
	runOK(t, ctx, installer, workspaceOf(t, ctx, db, next.WorkspaceID),
		`! grep -q 'rolled back' README.md`)

	// Rolling forward still works: the rolled-back commit was kept.
	forward, err := installer.Rollback(ctx, rolled)
	if err != nil {
		t.Fatalf("rolling forward: %v", err)
	}
	if forward.Revision != promoted.Revision {
		t.Fatalf("rolled forward to %s, want %s", forward.Revision, promoted.Revision)
	}
	if tip := revParse(t, bare, "refs/heads/main"); tip != promoted.Revision {
		t.Fatalf("after rolling forward main is %s, want %s", tip, promoted.Revision)
	}
}

func revParse(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-c", "safe.directory=*", "-C", bare, "rev-parse", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}
