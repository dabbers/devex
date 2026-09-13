package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/local"
)

func newLocalDriver(t *testing.T) *local.Driver {
	t.Helper()
	d, err := local.New(local.Options{
		Root:  t.TempDir(),
		Total: vm.Resources{VCPUs: 16, MemoryMiB: 32768, DiskGiB: 400},
	})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	return d
}

func TestEnsureUIVMCreatesOne(t *testing.T) {
	ctx := context.Background()
	driver := newLocalDriver(t)

	instance, err := EnsureUIVM(ctx, driver, VMConfig{})
	if err != nil {
		t.Fatalf("EnsureUIVM: %v", err)
	}
	if instance.State != vm.StateRunning {
		t.Fatalf("state = %q", instance.State)
	}
	if instance.Labels()[RoleLabel] != RoleUI {
		t.Fatalf("the UI VM is not labelled and could not be found again: %v", instance.Labels())
	}
	// It serves every fork, so it must not be owned by one.
	if instance.ForkID != "" {
		t.Fatalf("the shared UI VM is attached to fork %q", instance.ForkID)
	}
	if instance.Spec.Resources.MemoryMiB == 0 {
		t.Fatal("the UI VM was created with no memory")
	}
}

func TestEnsureUIVMIsIdempotentAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	driver := newLocalDriver(t)

	first, err := EnsureUIVM(ctx, driver, VMConfig{})
	if err != nil {
		t.Fatalf("EnsureUIVM: %v", err)
	}
	// A restarted control plane must recognise the machine it was already
	// using; nothing reclaims VMs, so booting a second one would strand the
	// first and quietly double the cost.
	second, err := EnsureUIVM(ctx, driver, VMConfig{})
	if err != nil {
		t.Fatalf("EnsureUIVM (repeat): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("a second UI VM was provisioned: %s then %s", first.ID, second.ID)
	}

	all, err := driver.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("driver holds %d instances, want exactly one UI VM", len(all))
	}
}

func TestEnsureUIVMReplacesAStoppedOne(t *testing.T) {
	ctx := context.Background()
	driver := newLocalDriver(t)

	first, err := EnsureUIVM(ctx, driver, VMConfig{})
	if err != nil {
		t.Fatalf("EnsureUIVM: %v", err)
	}
	if err := driver.Destroy(ctx, first.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	second, err := EnsureUIVM(ctx, driver, VMConfig{})
	if err != nil {
		t.Fatalf("EnsureUIVM after loss: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("expected a replacement to be provisioned")
	}
}

func TestFindUIVMIgnoresForkVMs(t *testing.T) {
	ctx := context.Background()
	driver := newLocalDriver(t)

	// A fork VM must never be mistaken for the shared UI VM: verification
	// would then run inside a workstream's own machine.
	if _, err := driver.Create(ctx, vm.Spec{
		ForkID: "fork_1", Name: "ratings", Image: "img",
		Resources: vm.Resources{VCPUs: 1, MemoryMiB: 1024, DiskGiB: 5},
		Labels:    map[string]string{"dabberz.fork": "fork_1"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	found, err := FindUIVM(ctx, driver)
	if err != nil {
		t.Fatalf("FindUIVM: %v", err)
	}
	if found != nil {
		t.Fatalf("a fork VM was mistaken for the UI VM: %+v", found)
	}
}

func TestEnsureUIVMReportsCapacityRefusal(t *testing.T) {
	ctx := context.Background()
	// Too small to fit the default UI VM.
	driver, err := local.New(local.Options{
		Root:  t.TempDir(),
		Total: vm.Resources{VCPUs: 1, MemoryMiB: 512, DiskGiB: 5},
	})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	// A machine with no room for the UI VM must say so rather than appearing
	// to succeed; verification is what gates every merge.
	if _, err := EnsureUIVM(ctx, driver, VMConfig{}); !errors.Is(err, vm.ErrNoCapacity) {
		t.Fatalf("EnsureUIVM = %v, want vm.ErrNoCapacity", err)
	}
}

func TestVMConfigDefaults(t *testing.T) {
	got := VMConfig{}.WithDefaults()
	if got.Name == "" || got.Image == "" {
		t.Fatalf("defaults are incomplete: %+v", got)
	}
	if got.Resources.MemoryMiB <= got.Resources.VCPUs*1024 {
		t.Fatalf("the UI VM should be memory-weighted for a browser: %s", got.Resources)
	}

	custom := VMConfig{Name: "mine", Image: "img", Resources: vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 10}}
	applied := custom.WithDefaults()
	if applied.Name != custom.Name || applied.Image != custom.Image || applied.Resources != custom.Resources {
		t.Fatalf("defaults overwrote explicit configuration: %+v", applied)
	}
}

func TestUIVMCarriesNoRepoSecrets(t *testing.T) {
	ctx := context.Background()
	driver := newLocalDriver(t)

	instance, err := EnsureUIVM(ctx, driver, VMConfig{})
	if err != nil {
		t.Fatalf("EnsureUIVM: %v", err)
	}
	// The UI VM is shared across every repo, so nothing repo-scoped may reach
	// it. Only explicitly configured environment belongs here.
	if len(instance.Spec.Env) != 0 {
		t.Fatalf("the shared UI VM was given environment it should not have: %v", instance.Spec.Env)
	}
}
