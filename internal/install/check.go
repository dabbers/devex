package install

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
)

// Checking a change before anybody is allowed to put it live.
//
// Two different questions get asked, and both matter. The app's own suite asks
// whether the paths that cost a customer money still work -- submit, store,
// notify -- which only the app's author can know. The browser harness asks
// whether the thing renders at all over its public address, which no unit test
// notices and which is the most common way a change breaks something.

// checkTimeout bounds one suite.
const checkTimeout = 20 * time.Minute

// What the platform tells a check suite. One pair of names for every app,
// because a suite that invents its own can be handed the wrong database by a
// deployment that spelled it differently.
const (
	// CheckURLEnv is the running preview to check.
	CheckURLEnv = "DABBERZ_CHECK_URL"
	// CheckDatabaseEnv is a throwaway database the suite may do anything to,
	// including empty it. Never the change's own copy: a suite that truncates
	// tables would leave the preview blank and look like it had worked.
	CheckDatabaseEnv = "DABBERZ_CHECK_DATABASE_URL"
)

// Check runs a change's critical-path suite and records what it found.
func (i *Installer) Check(ctx context.Context, change *domain.Change) (*domain.Change, error) {
	switch change.State {
	// Conflicted too: once its agent has resolved the conflict, checking it
	// is how it gets back to ready.
	case domain.ChangeOpen, domain.ChangeReady, domain.ChangeBlocked, domain.ChangeConflicted:
	default:
		return nil, fmt.Errorf("%w: a change is checked while it is open, not while it is %s",
			store.ErrConflict, change.State)
	}

	installation, err := i.deps.Store.GetInstallation(ctx, change.InstallationID)
	if err != nil {
		return nil, err
	}
	app, err := i.deps.Catalog.Get(installation.App)
	if err != nil {
		return nil, err
	}
	working, err := i.deps.Store.GetWorkspace(ctx, change.WorkspaceID)
	if err != nil {
		return nil, err
	}

	// What is about to be checked, read from the workspace rather than
	// remembered from when the change was opened. An agent that has committed
	// since then has moved the branch on, and checking one revision while
	// recording another is how unchecked code reaches a customer.
	revision, err := i.deps.Workspaces.Revision(ctx, working)
	if err != nil {
		return nil, err
	}
	change.Revision = revision

	// A conflict is over once production is part of the branch, however it
	// got there. Until then the files stay recorded: the next promote will
	// meet the same conflict, and the customer should see why beforehand.
	if len(change.Conflicts) > 0 {
		upToDate, err := i.deps.Workspaces.Contains(ctx, working, installation.Revision)
		if err != nil {
			return nil, err
		}
		if upToDate {
			change.Conflicts = nil
		}
	}

	// The preview is rebuilt on that revision before either check looks at
	// it. Without this the app's suite compiles the source as it stands while
	// the browser opens a binary built when the change was opened, and the two
	// results describe different code.
	if err := i.deps.Workspaces.Rebuild(ctx, working); err != nil {
		return nil, i.blocked(ctx, change, err)
	}

	change.State = domain.ChangeChecking
	change.Checks = nil
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return nil, err
	}

	results := []domain.CheckResult{
		i.checkSuite(ctx, app, installation, change, working),
	}
	if touched := i.checkedItsOwnGate(ctx, installation, change, working); touched != nil {
		results = append(results, *touched)
	}
	if i.deps.Verifier != nil {
		results = append(results, i.driveBrowser(ctx, change))
	}

	change.Checks = results
	if change.Passed() {
		change.State, change.StateReason = domain.ChangeReady, ""
	} else {
		change.State = domain.ChangeBlocked
		change.StateReason = "its checks did not pass"
	}
	if err := i.deps.Store.UpdateChange(ctx, change); err != nil {
		return nil, err
	}
	return change, nil
}

// checkSuite runs the app's own critical-path tests against the change.
func (i *Installer) checkSuite(ctx context.Context, app App, installation *domain.Installation, change *domain.Change, working *domain.Workspace) domain.CheckResult {
	result := domain.CheckResult{Name: "critical-path"}

	suite := strings.TrimSpace(app.Spec.Checks.Suite)
	if suite == "" {
		result.Passed = true
		result.Detail = "this app declares no critical-path suite"
		return result
	}

	env := map[string]string{CheckURLEnv: change.PreviewURL}
	if installation.Database != "" && i.deps.Databases != nil {
		// Its own database, emptied and thrown away. The change's copy is what
		// the preview is serving, and a suite that truncates tables would
		// leave it blank.
		scratch, dsn, err := i.deps.Databases.Provision(ctx, change.ID+"-check")
		if err != nil {
			result.Detail = "could not give the checks a database: " + err.Error()
			return result
		}
		defer func() {
			if err := i.deps.Databases.Drop(ctx, scratch); err != nil {
				i.deps.Logger.Error("could not drop a check database",
					"change", change.ID, "database", scratch.Name, "error", err)
			}
		}()
		env[CheckDatabaseEnv] = dsn
	}

	// Run from the app root, by the path the manifest declares. A suite that
	// is not executable is a manifest that is wrong, and saying so beats a
	// shell error nobody can place.
	command := "./" + strings.TrimSuffix(strings.TrimPrefix(suite, "./"), "/") + "/run.sh"
	res, err := i.deps.Workspaces.Run(ctx, working, command, env, checkTimeout)
	switch {
	case err != nil:
		result.Detail = "the suite could not be run: " + err.Error()
	case res.TimedOut:
		result.Detail = fmt.Sprintf("the suite did not finish within %s", checkTimeout)
	case res.ExitCode != 0:
		// The output is the diagnosis and the only thing worth showing.
		result.Detail = tailOf(res.Stdout+res.Stderr, 4000)
	default:
		result.Passed = true
		result.Detail = "the app's critical paths still work"
	}
	return result
}

// checkedItsOwnGate reports whether the change altered the suite judging it.
//
// Not forbidden: a check sometimes has to change with the code it checks, and
// a platform that refuses would just be routed around. But a change that
// rewrites its own gate is a thing a person should be told about before they
// promote it, rather than something they find out afterwards.
func (i *Installer) checkedItsOwnGate(ctx context.Context, installation *domain.Installation, change *domain.Change, working *domain.Workspace) *domain.CheckResult {
	spec, err := i.deps.Catalog.Get(installation.App)
	if err != nil || strings.TrimSpace(spec.Spec.Checks.Suite) == "" {
		return nil
	}
	if installation.Revision == "" {
		return nil
	}

	touched, err := i.deps.Workspaces.Changed(ctx, working, installation.Revision, spec.Spec.Checks.Suite)
	if err != nil {
		i.deps.Logger.Warn("could not tell whether a change edited its own checks",
			"change", change.ID, "error", err)
		return nil
	}
	if len(touched) == 0 {
		return nil
	}
	return &domain.CheckResult{
		Name:   "checks-unmodified",
		Passed: false,
		Detail: "this change edits the suite that judges it (" + strings.Join(touched, ", ") +
			"). That is sometimes right, and it means the result below is not independent.",
	}
}

// driveBrowser checks the preview the way a person would open it.
func (i *Installer) driveBrowser(ctx context.Context, change *domain.Change) domain.CheckResult {
	result := domain.CheckResult{Name: "renders"}
	if change.PreviewURL == "" {
		result.Detail = "this change has no preview to look at"
		return result
	}

	pass, err := i.ownersPass(ctx, change)
	if err != nil {
		result.Detail = "the preview could not be checked as its owner: " + err.Error()
		return result
	}
	report, err := i.deps.Verifier.Verify(ctx, verify.Request{
		ForkID:     change.ID,
		PreviewURL: change.PreviewURL,
		Intent:     change.Title,
		Pass:       pass,
	})
	if err != nil {
		result.Detail = "the preview could not be checked: " + err.Error()
		return result
	}
	result.Passed = report.Passed
	result.Detail = report.FeedbackText()
	if report.Passed {
		result.Detail = report.Summary
	}
	return result
}

// passLifetime bounds a pass to roughly one check. Long enough for a slow
// browser on a cold preview, short enough that a pass found later is spent.
const passLifetime = 15 * time.Minute

// ownersPass mints a pass for the person the change belongs to, valid at its
// preview alone. Empty, with no error, when this installer has no issuer: the
// check then looks as a stranger, as it always used to.
func (i *Installer) ownersPass(ctx context.Context, change *domain.Change) (string, error) {
	if i.deps.Passes == nil {
		return "", nil
	}
	preview, err := url.Parse(change.PreviewURL)
	if err != nil || preview.Hostname() == "" {
		return "", fmt.Errorf("%q is not a preview URL", change.PreviewURL)
	}
	owner, err := i.deps.Store.GetUser(ctx, change.UserID)
	if err != nil {
		return "", fmt.Errorf("look up the change's owner: %w", err)
	}
	return i.deps.Passes.Mint(preview.Hostname(), owner, passLifetime)
}

// tailOf keeps the end of a long output, which is where a suite says what
// failed.
func tailOf(output string, limit int) string {
	output = strings.TrimSpace(output)
	if len(output) <= limit {
		return output
	}
	return "…" + output[len(output)-limit:]
}
