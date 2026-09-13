package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/dabbers/devex/internal/vm"
)

// RoleLabel marks an instance's purpose. The shared UI VM is found by this
// label rather than by a remembered id, so the control plane can restart and
// still recognise the machine it was already using.
const RoleLabel = "dabberz.role"

// RoleUI is the label value carried by the shared UI VM.
const RoleUI = "ui"

// DefaultUIVMName is the instance name given to the shared UI VM.
const DefaultUIVMName = "dabberz-ui"

// VMConfig describes the shared UI VM to provision.
//
// There is one of these for the whole machine, not one per verifier: it runs a
// single browser with N independently addressable profiles, so it is sized for
// a browser and its profiles rather than for a workload.
type VMConfig struct {
	// AutoProvision creates the UI VM at startup when one is not already
	// running. Turning it off means supplying UIInstanceID by hand.
	AutoProvision bool `yaml:"auto_provision" json:"auto_provision"`
	// Name is the instance name, also used to recognise an existing VM.
	Name string `yaml:"name" json:"name"`
	// Image is the golden image the UI VM boots. It is the same generic image
	// forks boot, which is what carries the browser stack.
	Image string `yaml:"image" json:"image"`
	// Resources sizes the UI VM. A browser driving several profiles wants more
	// memory than a typical fork and fewer vCPUs.
	Resources vm.Resources `yaml:"resources" json:"resources"`
	// Env is injected into the UI VM, for whatever the verification harness
	// needs. Repo secrets deliberately do not reach it: it is shared across
	// every repo, so nothing repo-scoped belongs there.
	Env map[string]string `yaml:"env" json:"env"`
}

// WithDefaults fills unset fields.
func (c VMConfig) WithDefaults() VMConfig {
	if c.Name == "" {
		c.Name = DefaultUIVMName
	}
	if c.Image == "" {
		c.Image = "dabberz-golden"
	}
	if c.Resources == (vm.Resources{}) {
		c.Resources = vm.Resources{VCPUs: 2, MemoryMiB: 8192, DiskGiB: 20}
	}
	return c
}

// ErrNoUIVM reports that no shared UI VM is available, so nothing can be
// verified and therefore nothing can reach the merge gate.
var ErrNoUIVM = errors.New("verify: no shared UI VM available")

// EnsureUIVM returns the shared UI VM, creating it if one is not already
// running.
//
// It is idempotent across control-plane restarts: an existing VM is recognised
// by its role label rather than by a remembered id, so restarting does not
// strand the old machine and boot a second one. That matters more here than
// elsewhere, because nothing reclaims VMs automatically.
func EnsureUIVM(ctx context.Context, driver vm.Driver, cfg VMConfig) (*vm.Instance, error) {
	cfg = cfg.WithDefaults()

	existing, err := FindUIVM(ctx, driver)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	instance, err := driver.Create(ctx, vm.Spec{
		Name:      cfg.Name,
		Resources: cfg.Resources,
		Image:     cfg.Image,
		Env:       cfg.Env,
		// ForkID is deliberately empty: this machine serves every fork, so it
		// belongs to none of them and must not be torn down with one.
		Labels: map[string]string{RoleLabel: RoleUI},
	})
	if err != nil {
		return nil, fmt.Errorf("verify: provision the shared UI VM: %w", err)
	}
	return instance, nil
}

// FindUIVM returns the running shared UI VM, or nil if there is none.
func FindUIVM(ctx context.Context, driver vm.Driver) (*vm.Instance, error) {
	instances, err := driver.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify: list instances: %w", err)
	}
	for _, instance := range instances {
		if instance.Labels()[RoleLabel] != RoleUI {
			continue
		}
		// A stopped or failed UI VM is not usable; leave it in place (nothing
		// is reclaimed automatically) and provision a replacement.
		if instance.State == vm.StateRunning || instance.State == vm.StatePending {
			return instance, nil
		}
	}
	return nil, nil
}
