package install

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// Several changes open against one installation at once, going live in the
// order the customer says.
//
// The harness's driver has room for four machines (8 vCPUs at 2 each) and the
// installation itself takes one, so three changes fit and a fourth does not.

// TestSeveralChangesAtOnceUntilTheHostIsFull: each change gets its own
// branch, machine, preview and copy of the data, and one more than the host
// can fit is refused before anything is spent on it.
func TestSeveralChangesAtOnceUntilTheHostIsFull(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	installer, db, owner, _, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "busy")

	var open []*domain.Change
	seen := map[string]bool{}
	for _, title := range []string{"Add a checkout page", "Add analytics", "Reword the thank-you page"} {
		change, err := installer.OpenChange(ctx, installation, title)
		if err != nil {
			t.Fatalf("OpenChange(%q): %v", title, err)
		}
		for _, own := range []string{change.Branch, change.WorkspaceID, change.Database, change.PreviewURL} {
			if own == "" || seen[own] {
				t.Fatalf("change %q shares or lacks something of its own: %+v", title, change)
			}
			seen[own] = true
		}
		if change.BaseRevision == "" || change.BaseRevision != installation.Revision {
			t.Fatalf("change %q started from %q, production is at %q",
				title, change.BaseRevision, installation.Revision)
		}
		open = append(open, change)
	}

	// The fourth does not fit, and is refused rather than half made.
	change, err := installer.OpenChange(ctx, installation, "One too many")
	if err == nil {
		t.Fatal("a fourth change was opened on a host with room for three")
	}
	if !errors.Is(err, vm.ErrNoCapacity) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if change != nil {
		t.Fatalf("a refused change came back as if it existed: %+v", change)
	}
	recorded, err := db.store.ListChanges(ctx, installation.ID, 50)
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if len(recorded) != len(open) {
		t.Fatalf("%d changes recorded after refusing one; it left %d behind",
			len(recorded), len(recorded)-len(open))
	}

	// Finishing one makes room.
	if err := installer.Abandon(ctx, open[0]); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if _, err := installer.OpenChange(ctx, installation, "One too many"); err != nil {
		t.Fatalf("OpenChange once there was room: %v", err)
	}
}

// TestChangesGoLiveOneAfterAnother: two changes that touch different files,
// promoted at the same moment. One goes first; the other finds production has
// moved, takes it in, is checked again against fresh data, and follows. Live
// ends up with both.
func TestChangesGoLiveOneAfterAnother(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	installer, db, owner, edge, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "orderly")
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	first := openAndCommit(t, ctx, installer, db, installation, "Add a checkout page",
		`echo '// checkout page' >> main.go`)
	second := openAndCommit(t, ctx, installer, db, installation, "Add analytics",
		`echo '// analytics' >> store.go`)
	opened := map[string]string{first.ID: first.Database, second.ID: second.Database}

	// Production takes a real form after both changes forked their data, so
	// whichever goes second has a copy that predates it.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	if status := post(t, client, serviceAddress(t, db, installation), "", "/forms", url.Values{
		"title":  {"Arrived later"},
		"fields": {"Your name | text"},
	}, asOwner(owner.ID)); status != http.StatusSeeOther {
		t.Fatalf("creating a form in production returned %d", status)
	}

	markReady(t, ctx, installer, db, first)
	markReady(t, ctx, installer, db, second)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, change := range []*domain.Change{first, second} {
		wg.Add(1)
		go func(n int, change *domain.Change) {
			defer wg.Done()
			_, errs[n] = installer.Promote(ctx, change)
		}(n, change)
	}
	wg.Wait()
	for n, err := range errs {
		if err != nil {
			change := []*domain.Change{first, second}[n]
			t.Fatalf("promoting %q: %v\nits checks: %+v", change.Title, err, change.Checks)
		}
	}

	// Which went first is the scheduler's business; that the other followed
	// it rather than replacing it is this test's.
	earlier, later := first, second
	if later.PromotedAt.Before(*earlier.PromotedAt) {
		earlier, later = later, earlier
	}

	live, err := db.store.GetInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	if live.Revision != later.Revision || live.PreviousRevision != earlier.Revision {
		t.Fatalf("production is at %s after %s; want %s after %s",
			short(live.Revision), short(live.PreviousRevision), short(later.Revision), short(earlier.Revision))
	}
	serving := workspaceOf(t, ctx, db, live.WorkspaceID)
	runOK(t, ctx, installer, serving, `grep -q '// checkout page' main.go && grep -q '// analytics' store.go`)

	// The one that followed was checked again, on the merged commit, rather
	// than going live on the strength of checks its own commit passed.
	if detailOf(later.Checks, "critical-path") == staleCheck {
		t.Fatal("the change that followed went live on checks from before it was brought up to date")
	}
	if earlier.Checks[0].Detail != staleCheck {
		t.Fatal("the change that went first was checked again for no reason")
	}

	// Against a new copy of production's data, not the one it opened with.
	if later.Database == opened[later.ID] {
		t.Fatal("the re-check ran against the change's original copy of the data, which predates production's")
	}
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")
}

// TestConflictingChangesGoToTheLaterChangesAgent: two changes edit the same
// lines. The first goes live; the second does not, is marked conflicted with
// the file named, and is left exactly as it was for its own agent. Once that
// agent has resolved it, it is checked and goes live as usual.
func TestConflictingChangesGoToTheLaterChangesAgent(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	installer, db, owner, edge, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "contested")
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	first := openAndCommit(t, ctx, installer, db, installation, "Add a checkout page",
		`echo 'Now with a checkout page.' > README.md`)
	second := openAndCommit(t, ctx, installer, db, installation, "Add PostHog tracking",
		`echo 'Now with PostHog tracking.' > README.md`)
	markReady(t, ctx, installer, db, first)
	markReady(t, ctx, installer, db, second)

	if _, err := installer.Promote(ctx, first); err != nil {
		t.Fatalf("promoting the first: %v", err)
	}
	live, err := db.store.GetInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}

	// An edit nobody committed is not promoted, and is not swept into a merge.
	working := workspaceOf(t, ctx, db, second.WorkspaceID)
	runOK(t, ctx, installer, working, `echo '// not committed' >> main.go`)
	if _, err := installer.Promote(ctx, second); err == nil || !strings.Contains(err.Error(), "not committed") {
		t.Fatalf("a change with uncommitted edits was not refused for them: %v", err)
	}
	runOK(t, ctx, installer, working, `git checkout -- main.go`)

	// The second conflicts, and nothing goes live.
	checkedAt := second.Revision
	_, err = installer.Promote(ctx, second)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("promoting a conflicting change: %v", err)
	}
	if second.State != domain.ChangeConflicted {
		t.Fatalf("a conflicting change is %s, not conflicted (%s)", second.State, second.StateReason)
	}
	if len(second.Conflicts) != 1 || second.Conflicts[0] != "README.md" {
		t.Fatalf("recorded conflicts %v, want [README.md]", second.Conflicts)
	}
	stored, err := db.store.GetChange(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetChange: %v", err)
	}
	if stored.State != domain.ChangeConflicted || len(stored.Conflicts) != 1 {
		t.Fatalf("the conflict was not recorded: %s %v", stored.State, stored.Conflicts)
	}
	after, err := db.store.GetInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	if after.Revision != live.Revision {
		t.Fatal("a conflicting change went live")
	}

	// Left as it was: its own commit, no merge half done.
	runOK(t, ctx, installer, working, `! git rev-parse -q --verify MERGE_HEAD && ! grep -q '<<<<<<<' README.md`)
	if head, err := installer.deps.Workspaces.Revision(ctx, working); err != nil || head != checkedAt {
		t.Fatalf("the conflicted change's branch moved to %s (%v)", short(head), err)
	}

	// And it cannot be pushed through by asking again.
	if _, err := installer.Promote(ctx, second); err == nil {
		t.Fatal("a conflicted change was promoted")
	}

	// What its agent is told: what went live, where it conflicts, and what
	// to merge.
	brief, err := installer.ConflictBrief(ctx, second)
	if err != nil {
		t.Fatalf("ConflictBrief: %v", err)
	}
	for _, want := range []string{first.Title, "README.md", live.Revision, "git merge"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the brief does not mention %q:\n%s", want, brief)
		}
	}

	// The agent resolves it, keeping both.
	runOK(t, ctx, installer, working, `git fetch origin && `+
		`(git -c user.email=t@example.invalid -c user.name=t merge `+live.Revision+` || true) && `+
		`git rev-parse -q --verify MERGE_HEAD && `+
		`printf 'Now with a checkout page.\nNow with PostHog tracking.\n' > README.md && `+
		`git add README.md && `+
		`git -c user.email=t@example.invalid -c user.name=t commit --no-edit`)

	checked, err := installer.Check(ctx, second)
	if err != nil {
		t.Fatalf("Check after resolving: %v", err)
	}
	if checked.State != domain.ChangeReady {
		t.Fatalf("the resolved change is %s: %+v", checked.State, checked.Checks)
	}
	if len(checked.Conflicts) != 0 {
		t.Fatalf("a resolved change still lists conflicts: %v", checked.Conflicts)
	}
	if _, err := installer.ConflictBrief(ctx, checked); err == nil {
		t.Fatal("a change with no conflict produced a conflict brief")
	}

	promoted, err := installer.Promote(ctx, checked)
	if err != nil {
		t.Fatalf("promoting the resolved change: %v", err)
	}
	if promoted.Revision != checked.Revision || promoted.PreviousRevision != live.Revision {
		t.Fatalf("production is at %s after %s", short(promoted.Revision), short(promoted.PreviousRevision))
	}
	serving := workspaceOf(t, ctx, db, promoted.WorkspaceID)
	runOK(t, ctx, installer, serving,
		`grep -q 'checkout page' README.md && grep -q 'PostHog' README.md`)
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")
}

// --------------------------------------------------------------- helpers

// staleCheck marks checks a test recorded by hand, so that a later check
// having really run can be told apart from one that did not.
const staleCheck = "recorded by the test, not run"

// openAndCommit opens a change and commits one edit to it.
func openAndCommit(t *testing.T, ctx context.Context, installer *Installer, db *tracked, installation *domain.Installation, title, edit string) *domain.Change {
	t.Helper()
	change, err := installer.OpenChange(ctx, installation, title)
	if err != nil {
		t.Fatalf("OpenChange(%q): %v", title, err)
	}
	runOK(t, ctx, installer, workspaceOf(t, ctx, db, change.WorkspaceID), edit+` && `+
		`git -c user.email=t@example.invalid -c user.name=t commit -qam '`+title+`'`)
	return change
}

// markReady records a change as having passed its checks on its current
// commit, without spending minutes running them. A promote that has to bring
// it up to date runs them for real.
func markReady(t *testing.T, ctx context.Context, installer *Installer, db *tracked, change *domain.Change) {
	t.Helper()
	revision, err := installer.deps.Workspaces.Revision(ctx, workspaceOf(t, ctx, db, change.WorkspaceID))
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	change.Revision = revision
	change.State = domain.ChangeReady
	change.Checks = []domain.CheckResult{{Name: "critical-path", Passed: true, Detail: staleCheck}}
	if err := db.store.UpdateChange(ctx, change); err != nil {
		t.Fatalf("UpdateChange: %v", err)
	}
}

func workspaceOf(t *testing.T, ctx context.Context, db *tracked, id string) *domain.Workspace {
	t.Helper()
	ws, err := db.store.GetWorkspace(ctx, id)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	return ws
}

// runOK runs a shell command in a workspace and fails the test unless it
// exits zero.
func runOK(t *testing.T, ctx context.Context, installer *Installer, ws *domain.Workspace, command string) {
	t.Helper()
	res, err := installer.deps.Workspaces.Run(ctx, ws, command, nil, 2*time.Minute)
	if err != nil {
		t.Fatalf("%s: %v", command, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("%s: exit %d\n%s%s", command, res.ExitCode, res.Stdout, res.Stderr)
	}
}
