package install

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/workspace"
)

// releaseEverything is the harness's cleanup: every workspace the test's
// store knows about is released, which stops its services and destroys its
// machine. The local driver's Destroy only removes the directory, so a
// service nobody stopped outlives the test -- which is how a run of this
// package once left ~50 `forms serve` behind.
//
// It runs whether the test passed or failed, and then checks that nothing is
// still running from the test's directory. Anything found fails the test and
// is killed, so the check cannot itself leak.
func releaseEverything(t *testing.T, db *store.Store, workspaces *workspace.Manager, dataDir string) {
	t.Helper()

	ctx := context.Background()
	all, err := db.ListWorkspaces(ctx, store.WorkspaceFilter{IncludeEphemeral: true})
	if err != nil {
		t.Errorf("listing workspaces to release: %v", err)
	}
	for _, w := range all {
		if err := workspaces.Release(ctx, w); err != nil {
			t.Errorf("releasing workspace %s: %v", w.ID, err)
		}
	}

	for _, p := range processesUnder(dataDir) {
		t.Errorf("left running after the test: pid %d (%s) in %s", p.pid, p.cmdline, p.cwd)
		if proc, err := os.FindProcess(p.pid); err == nil {
			_ = proc.Kill()
		}
	}
}

type straggler struct {
	pid     int
	cwd     string
	cmdline string
}

// processesUnder lists processes whose working directory is inside dir.
// Services run from the instance's workspace, which is inside the test's data
// directory, so this finds exactly the ones this test started and no other
// session's. Where there is no /proc it finds nothing.
func processesUnder(dir string) []straggler {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	// Resolve symlinks (t.TempDir under a symlinked /tmp) so the prefix matches
	// what /proc reports.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}

	var found []straggler
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if cwd != dir && !strings.HasPrefix(cwd, dir+string(filepath.Separator)) {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		found = append(found, straggler{pid, cwd, strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))})
	}
	return found
}
