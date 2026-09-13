package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/vm"
)

func newDriver(t *testing.T, total vm.Resources) *Driver {
	t.Helper()
	d, err := New(Options{Root: t.TempDir(), Total: total})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func smallSpec(name string) vm.Spec {
	return vm.Spec{
		Name:      name,
		Image:     "dabberz-golden",
		Resources: vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10},
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	if _, err := New(Options{Total: vm.Resources{VCPUs: 1, MemoryMiB: 1, DiskGiB: 1}}); err == nil {
		t.Error("expected an error when root is empty")
	}
	if _, err := New(Options{Root: t.TempDir()}); err == nil {
		t.Error("expected an error when capacity is zero")
	}
}

func TestCreateRejectsInvalidSpec(t *testing.T) {
	d := newDriver(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100})
	for name, spec := range map[string]vm.Spec{
		"no name":   {Image: "img", Resources: vm.Resources{VCPUs: 1, MemoryMiB: 1, DiskGiB: 1}},
		"no image":  {Name: "n", Resources: vm.Resources{VCPUs: 1, MemoryMiB: 1, DiskGiB: 1}},
		"no cpu":    {Name: "n", Image: "img", Resources: vm.Resources{MemoryMiB: 1, DiskGiB: 1}},
		"no memory": {Name: "n", Image: "img", Resources: vm.Resources{VCPUs: 1, DiskGiB: 1}},
		"no disk":   {Name: "n", Image: "img", Resources: vm.Resources{VCPUs: 1, MemoryMiB: 1}},
	} {
		if _, err := d.Create(context.Background(), spec); err == nil {
			t.Errorf("Create(%s) should have failed validation", name)
		}
	}
}

func TestCreateAndExec(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100})

	spec := smallSpec("fork-ratings")
	spec.ForkID = "fork_abc"
	spec.Env = map[string]string{"DABBERZ_SECRET": "swordfish"}

	inst, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.State != vm.StateRunning {
		t.Fatalf("state = %q, want %q", inst.State, vm.StateRunning)
	}
	if _, err := os.Stat(inst.Workspace); err != nil {
		t.Fatalf("workspace not created: %v", err)
	}

	// Commands default to the instance workspace.
	res, err := d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"pwd"}})
	if err != nil {
		t.Fatalf("Exec(pwd): %v", err)
	}
	if !res.OK() {
		t.Fatalf("pwd failed: %+v", res)
	}
	// macOS and some Linux setups symlink temp dirs, so compare the resolved paths.
	gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	wantDir, _ := filepath.EvalSymlinks(inst.Workspace)
	if gotDir != wantDir {
		t.Fatalf("pwd = %q, want %q", gotDir, wantDir)
	}

	// Instance environment reaches the command.
	res, err = d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c", "echo $DABBERZ_SECRET"}})
	if err != nil {
		t.Fatalf("Exec(echo): %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "swordfish" {
		t.Fatalf("instance env not injected: %q", res.Stdout)
	}

	// Per-command environment overrides the instance's.
	res, err = d.Exec(ctx, inst.ID, vm.Command{
		Argv: []string{"sh", "-c", "echo $DABBERZ_SECRET"},
		Env:  map[string]string{"DABBERZ_SECRET": "override"},
	})
	if err != nil {
		t.Fatalf("Exec(override): %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "override" {
		t.Fatalf("command env did not override: %q", res.Stdout)
	}
}

func TestExecReportsNonZeroExitWithoutError(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50})
	inst, err := d.Create(ctx, smallSpec("fork-x"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A failing workload is a result, not a driver error: the verify/fix loop
	// depends on being able to read the exit code and stderr.
	res, err := d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c", "echo boom >&2; exit 3"}})
	if err != nil {
		t.Fatalf("Exec returned a driver error for a failing command: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("stderr = %q, want it to contain \"boom\"", res.Stderr)
	}
	if res.OK() {
		t.Fatal("OK() should be false for a non-zero exit")
	}
}

func TestExecHonoursTimeout(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50})
	inst, err := d.Create(ctx, smallSpec("fork-slow"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := d.Exec(ctx, inst.ID, vm.Command{
		Argv:    []string{"sleep", "30"},
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected the command to time out, got %+v", res)
	}
	if res.OK() {
		t.Fatal("a timed-out command is not OK")
	}
}

func TestExecStdinIsDelivered(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50})
	inst, err := d.Create(ctx, smallSpec("fork-stdin"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"cat"}, Stdin: []byte("prompt text")})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "prompt text" {
		t.Fatalf("stdout = %q, want the stdin echoed back", res.Stdout)
	}
}

func TestExecUnknownInstance(t *testing.T) {
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50})
	if _, err := d.Exec(context.Background(), "vm_missing", vm.Command{Argv: []string{"true"}}); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Exec(missing) = %v, want vm.ErrNotFound", err)
	}
}

func TestCapacityIsExhaustedRatherThanOversubscribed(t *testing.T) {
	ctx := context.Background()
	// Room for exactly two of the small spec.
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 20})

	for i := range 2 {
		if _, err := d.Create(ctx, smallSpec("fork")); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if _, err := d.Create(ctx, smallSpec("fork-overflow")); !errors.Is(err, vm.ErrNoCapacity) {
		t.Fatalf("third Create = %v, want vm.ErrNoCapacity", err)
	}

	capacity, err := d.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if capacity.Instances != 2 {
		t.Fatalf("instances = %d, want 2", capacity.Instances)
	}
	if free := capacity.Free(); free.VCPUs != 0 || free.MemoryMiB != 0 {
		t.Fatalf("free = %s, want nothing left", free)
	}
}

func TestMaxInstancesCapsIndependentlyOfResources(t *testing.T) {
	ctx := context.Background()
	d, err := New(Options{
		Root:         t.TempDir(),
		Total:        vm.Resources{VCPUs: 64, MemoryMiB: 65536, DiskGiB: 1000},
		MaxInstances: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Create(ctx, smallSpec("first")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := d.Create(ctx, smallSpec("second")); !errors.Is(err, vm.ErrNoCapacity) {
		t.Fatalf("second Create = %v, want vm.ErrNoCapacity despite spare resources", err)
	}
}

func TestDestroyReleasesCapacityAndRemovesWorkspace(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10})

	inst, err := d.Create(ctx, smallSpec("fork-temp"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.Destroy(ctx, inst.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(inst.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace still present after Destroy (err = %v)", err)
	}
	if _, err := d.Get(ctx, inst.ID); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Get after Destroy = %v, want vm.ErrNotFound", err)
	}
	// The slot must be reusable.
	if _, err := d.Create(ctx, smallSpec("fork-replacement")); err != nil {
		t.Fatalf("Create after Destroy: %v", err)
	}
	if err := d.Destroy(ctx, "vm_missing"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Destroy(missing) = %v, want vm.ErrNotFound", err)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50})
	spec := smallSpec("fork-copy")
	spec.Env = map[string]string{"K": "v"}
	inst, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	inst.Name = "mutated"
	inst.Spec.Env["K"] = "tampered"

	fresh, err := d.Get(ctx, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.Name != "fork-copy" || fresh.Spec.Env["K"] != "v" {
		t.Fatalf("driver state was mutated through a returned instance: %+v", fresh)
	}
}

func TestConcurrentCreateRespectsCapacity(t *testing.T) {
	ctx := context.Background()
	// Room for exactly four.
	d := newDriver(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 40})

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		rejected int
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.Create(ctx, smallSpec("racer"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, vm.ErrNoCapacity):
				rejected++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if created != 4 {
		t.Fatalf("created %d instances, want exactly 4", created)
	}
	if rejected != 12 {
		t.Fatalf("rejected %d, want 12", rejected)
	}
}

func TestListIncludesEveryInstance(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t, vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100})
	for range 3 {
		if _, err := d.Create(ctx, smallSpec("fork")); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	all, err := d.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List returned %d instances, want 3", len(all))
	}
}

func TestInstancesSurviveARestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	total := vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100}

	first, err := New(Options{Root: root, Total: total})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := smallSpec("fork-persistent")
	spec.ForkID = "fork_abc"
	spec.Env = map[string]string{"DABBERZ_SECRET": "kept"}
	created, err := first.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A new driver over the same root is what a control-plane restart looks
	// like. Nothing reclaims machines, so the fork's recorded machine must
	// still be there rather than orphaned.
	second, err := New(Options{Root: root, Total: total})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}

	found, err := second.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("the instance did not survive a restart: %v", err)
	}
	if found.ForkID != "fork_abc" || found.Name != "fork-persistent" {
		t.Fatalf("instance was not restored faithfully: %+v", found)
	}
	if found.Spec.Env["DABBERZ_SECRET"] != "kept" {
		t.Fatalf("instance environment was lost: %v", found.Spec.Env)
	}
	if found.Workspace != created.Workspace {
		t.Fatalf("workspace moved: %q vs %q", found.Workspace, created.Workspace)
	}

	// A restored instance is still usable, not just visible.
	res, err := second.Exec(ctx, created.ID, vm.Command{Argv: []string{"sh", "-c", "echo alive"}})
	if err != nil {
		t.Fatalf("Exec after restart: %v", err)
	}
	if !strings.Contains(res.Stdout, "alive") {
		t.Fatalf("stdout = %q", res.Stdout)
	}

	// And it counts against capacity again, or a restart would silently
	// oversubscribe the machine.
	capacity, err := second.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if capacity.Instances != 1 {
		t.Fatalf("instances = %d after restart, want 1", capacity.Instances)
	}
}

func TestRestartIgnoresUnrelatedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "not-an-instance"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	d, err := New(Options{Root: root, Total: vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	all, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("a directory with no instance record was adopted: %+v", all)
	}
}
