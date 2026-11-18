package install

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// TestAChangeGoesLiveWithoutTouchingProductionFirst is the loop the whole
// product is selling, start to finish: somebody asks for something, it runs
// somewhere that is not their live site, against a copy of their real data,
// and only then does a person put it live.
//
// Every claim in that sentence is checked here rather than assumed, because
// each one is a promise to somebody whose business is on the other end.
func TestAChangeGoesLiveWithoutTouchingProductionFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("installs an app and provisions two databases")
	}
	if os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	installer, db, owner, edge, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "live")
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	live := serviceAddress(t, db, installation)

	// Something real in production, which the change must be able to see and
	// must not be able to destroy.
	if status := post(t, client, live, "", "/forms", url.Values{
		"title":  {"Contact us"},
		"fields": {"Your name | text | required"},
	}, asOwner(owner.ID)); status != http.StatusSeeOther {
		t.Fatalf("creating the live form returned %d", status)
	}
	if status := post(t, client, edge, installation.URL, "/f/contact-us", url.Values{
		"your-name": {"A real customer"},
	}, nil); status != http.StatusOK {
		t.Fatalf("the live submission returned %d", status)
	}

	change, err := installer.OpenChange(ctx, installation, "Ask people for their company too")
	if err != nil {
		t.Fatalf("OpenChange: %v (%s)", err, change.StateReason)
	}
	if change.PreviewURL == "" || change.PreviewURL == installation.URL {
		t.Fatalf("the change has no address of its own: %q", change.PreviewURL)
	}
	if change.Database == "" || change.Database == installation.Database {
		t.Fatalf("the change shares production's database: %q", change.Database)
	}
	if change.Revision == "" {
		t.Fatal("the change records no revision, so there is nothing to promote")
	}
	waitForHealth(t, ctx, edge, change.PreviewURL, "/healthz")

	// The copy is real data. A change tested against an empty database has
	// not been tested.
	var copied string
	if err := db.QueryRow(ctx, change.Database,
		`SELECT data->>'your-name' FROM submissions LIMIT 1`, &copied); err != nil {
		t.Fatalf("reading the change's copy: %v", err)
	}
	if copied != "A real customer" {
		t.Fatalf("the change's copy holds %q, not production's data", copied)
	}

	// And it is a copy. Experimenting in the preview must not reach the thing
	// customers are using.
	preview := serviceAddress(t, db, &domain.Installation{WorkspaceID: change.WorkspaceID})
	if status := post(t, client, preview, "", "/forms", url.Values{
		"title":  {"Experiment"},
		"fields": {"Anything | text"},
	}, asOwner(owner.ID)); status != http.StatusSeeOther {
		t.Fatalf("creating a form in the preview returned %d", status)
	}
	var liveForms int
	if err := db.QueryRowInt(ctx, installation.Database,
		`SELECT count(*) FROM forms`, &liveForms); err != nil {
		t.Fatalf("counting production's forms: %v", err)
	}
	if liveForms != 1 {
		t.Fatalf("production has %d forms; the change leaked into it", liveForms)
	}

	// An unchecked change does not go live, however nicely it is asked.
	if _, err := installer.Promote(ctx, change); err == nil {
		t.Fatal("an unchecked change was promoted")
	} else if !strings.Contains(err.Error(), "checks") {
		t.Fatalf("the refusal does not say why: %v", err)
	}

	// Checks pass, and a person promotes.
	change.State = domain.ChangeReady
	change.Checks = []domain.CheckResult{{Name: "critical-path", Passed: true}}
	if err := db.store.UpdateChange(ctx, change); err != nil {
		t.Fatalf("UpdateChange: %v", err)
	}

	before := installation.Revision
	promoted, err := installer.Promote(ctx, change)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.Revision != change.Revision {
		t.Fatalf("installation is at %q, the change was %q", promoted.Revision, change.Revision)
	}
	if promoted.PreviousRevision != before {
		t.Fatalf("previous revision is %q, want %q", promoted.PreviousRevision, before)
	}

	// Production kept its data across the promote. Shipping a change is not
	// an occasion to lose a customer's submissions.
	var survived string
	if err := db.QueryRow(ctx, installation.Database,
		`SELECT data->>'your-name' FROM submissions LIMIT 1`, &survived); err != nil {
		t.Fatalf("reading production after the promote: %v", err)
	}
	if survived != "A real customer" {
		t.Fatalf("production holds %q after the promote", survived)
	}

	// And it is still serving, at the same address.
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	// Rolling back returns the code and leaves the data alone.
	rolled, err := installer.Rollback(ctx, promoted)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolled.Revision != before {
		t.Fatalf("after rolling back the installation is at %q, want %q", rolled.Revision, before)
	}
	if rolled.PreviousRevision != change.Revision {
		t.Fatal("rolling back is not itself undoable")
	}
	if err := db.QueryRow(ctx, installation.Database,
		`SELECT data->>'your-name' FROM submissions LIMIT 1`, &survived); err != nil {
		t.Fatalf("reading production after the rollback: %v", err)
	}
	if survived != "A real customer" {
		t.Fatal("rolling back the code took the data with it")
	}
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")
}

// TestOneChangeAtATime: two changes in flight against one installation means
// two previews, two scratch databases, and a promote that has to reason about
// what the other one did -- a merge conflict presented to somebody who does
// not know what git is.
func TestOneChangeAtATime(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	installer, _, owner, _, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "busy")

	first, err := installer.OpenChange(ctx, installation, "first")
	if err != nil {
		t.Fatalf("OpenChange: %v (%s)", err, first.StateReason)
	}
	if _, err := installer.OpenChange(ctx, installation, "second"); err == nil {
		t.Fatal("two changes were opened against one installation")
	} else if !strings.Contains(err.Error(), "already has a change open") {
		t.Fatalf("unhelpful refusal: %v", err)
	}

	// Abandoning the first lets another start.
	if err := installer.Abandon(ctx, first); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	second, err := installer.OpenChange(ctx, installation, "second")
	if err != nil {
		t.Fatalf("OpenChange after abandoning: %v (%s)", err, second.StateReason)
	}
}

// TestAChangeNeedsSomethingToChange covers the states a change cannot start
// from, which is most of them.
func TestAChangeNeedsSomethingToChange(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	installer, db, owner, _, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "stopped")

	installation.State = domain.InstallationStopped
	if err := db.store.UpdateInstallation(ctx, installation); err != nil {
		t.Fatalf("UpdateInstallation: %v", err)
	}
	if _, err := installer.OpenChange(ctx, installation, "something"); err == nil {
		t.Fatal("a change was opened against a stopped installation")
	}

	installation.State = domain.InstallationRunning
	if err := db.store.UpdateInstallation(ctx, installation); err != nil {
		t.Fatalf("UpdateInstallation: %v", err)
	}
	if _, err := installer.OpenChange(ctx, installation, "   "); err == nil {
		t.Fatal("a change with no title was opened")
	}
}

// --------------------------------------------------------------- helpers

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func asOwner(userID string) map[string]string {
	return map[string]string{"X-Dabberz-User": userID}
}

func installApp(t *testing.T, ctx context.Context, installer *Installer, userID, name string) *domain.Installation {
	t.Helper()

	installation, err := installer.Install(ctx, Request{UserID: userID, App: "forms", Name: name})
	if err != nil {
		t.Fatalf("Install: %v (%s)", err, installation.StateReason)
	}
	return installation
}

// TestChecksDecideWhetherAChangeCanGoLive runs the app's real critical-path
// suite against a real change, which is the gate the whole promote path
// depends on.
func TestChecksDecideWhetherAChangeCanGoLive(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	installer, db, owner, edge, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "checked")
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	change, err := installer.OpenChange(ctx, installation, "Tidy the wording")
	if err != nil {
		t.Fatalf("OpenChange: %v (%s)", err, change.StateReason)
	}

	checked, err := installer.Check(ctx, change)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checked.State != domain.ChangeReady {
		t.Fatalf("an untouched change did not pass its own suite: %s\n%s",
			checked.State, detailOf(checked.Checks, "critical-path"))
	}
	if !checked.Promotable() {
		t.Fatal("a change that passed its checks is not promotable")
	}

	// And a change that breaks the app does not get through.
	working, err := db.store.GetWorkspace(ctx, change.WorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	// Break the critical path in the way a careless change would: a
	// submission's answers stop being written. Nothing about the app looks
	// broken until somebody checks what was recorded.
	if _, err := installer.deps.Workspaces.Run(ctx, working,
		`sed -i 's|VALUES ($1, $2, $3)|VALUES ($1, $2, NULL)|' store.go`,
		nil, 2*time.Minute); err != nil {
		t.Fatalf("breaking the app: %v", err)
	}

	blocked, err := installer.Check(ctx, change)
	if err != nil {
		t.Fatalf("Check after breaking it: %v", err)
	}
	if blocked.State != domain.ChangeBlocked {
		t.Fatalf("a change that breaks the critical path was marked %s", blocked.State)
	}
	if blocked.Promotable() {
		t.Fatal("a failing change is promotable")
	}
	if _, err := installer.Promote(ctx, blocked); err == nil {
		t.Fatal("a failing change was promoted")
	}
}

// TestAChangeThatRewritesItsOwnGateIsReported: a check the change edited is
// not an independent result, and a person should be told before they promote
// rather than afterwards.
func TestAChangeThatRewritesItsOwnGateIsReported(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	installer, db, owner, edge, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "gatekeeper")
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	change, err := installer.OpenChange(ctx, installation, "Relax the checks")
	if err != nil {
		t.Fatalf("OpenChange: %v (%s)", err, change.StateReason)
	}
	working, err := db.store.GetWorkspace(ctx, change.WorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}

	// Edit the suite, and commit it so it is part of the change.
	if _, err := installer.deps.Workspaces.Run(ctx, working,
		`echo '# relaxed' >> checks/run.sh && `+
			`git -c user.email=t@example.invalid -c user.name=t commit -am 'relax the checks'`,
		nil, 2*time.Minute); err != nil {
		t.Fatalf("editing the suite: %v", err)
	}

	checked, err := installer.Check(ctx, change)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if detailOf(checked.Checks, "checks-unmodified") == "" {
		t.Fatalf("editing the gate was not reported: %+v", checked.Checks)
	}
	if checked.State != domain.ChangeBlocked {
		t.Fatalf("a change that rewrote its own gate was marked %s", checked.State)
	}
}

// TestWorkingOnAChangeAfterItPassedMeansCheckingItAgain is the claim that
// everything else here rests on.
//
// "Nothing goes live unchecked" is about a particular commit. A change that is
// worked on once more after being told it was ready -- one more commit from an
// agent that did not stop, a push by hand -- would otherwise promote code that
// passed nothing, while reporting that its checks were green.
func TestWorkingOnAChangeAfterItPassedMeansCheckingItAgain(t *testing.T) {
	if testing.Short() || os.Getenv(clusterEnv) == "" {
		t.Skip("set " + clusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	installer, db, owner, edge, _ := newInstaller(t, os.Getenv(clusterEnv))
	installation := installApp(t, ctx, installer, owner.ID, "latecomer")
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	change, err := installer.OpenChange(ctx, installation, "Say hello")
	if err != nil {
		t.Fatalf("OpenChange: %v (%s)", err, change.StateReason)
	}
	working, err := db.store.GetWorkspace(ctx, change.WorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}

	checked, err := installer.Check(ctx, change)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checked.State != domain.ChangeReady {
		t.Fatalf("a change with nothing wrong with it is %s: %+v", checked.State, checked.Checks)
	}
	passed := checked.Revision

	// One more commit, after being told it was ready.
	if _, err := installer.deps.Workspaces.Run(ctx, working,
		`echo '// later' >> main.go && `+
			`git -c user.email=t@example.invalid -c user.name=t commit -am 'one more thing'`,
		nil, 2*time.Minute); err != nil {
		t.Fatalf("committing again: %v", err)
	}

	if _, err := installer.Promote(ctx, checked); err == nil {
		t.Fatal("a change promoted code that its checks never saw")
	} else if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Promote refused for the wrong reason: %v", err)
	}

	// And checking again picks the new commit up, rather than re-blessing the
	// one that happened to be recorded.
	rechecked, err := installer.Check(ctx, change)
	if err != nil {
		t.Fatalf("Check again: %v", err)
	}
	if rechecked.Revision == passed {
		t.Fatalf("checking again judged the old revision %s", short(passed))
	}
	if _, err := installer.Promote(ctx, rechecked); err != nil {
		t.Fatalf("Promote after re-checking: %v", err)
	}
}

func detailOf(checks []domain.CheckResult, name string) string {
	for _, check := range checks {
		if check.Name == name {
			return check.Detail
		}
	}
	return ""
}
