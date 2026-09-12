// Package local implements a vm.Driver backed by host directories and
// processes rather than real machines.
//
// It exists so the orchestrator, scheduler, preview router and verify/fix loop
// can be developed and tested on any machine, including CI, where KVM and
// Firecracker are unavailable. It provides the same interface and the same
// capacity accounting as the production driver, but it provides no isolation
// whatsoever: commands run as the control-plane user on the host. It must
// never be used to run untrusted agent workloads.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/id"
	"github.com/dabbers/devex/internal/vm"
)

// DefaultExecTimeout bounds a command that did not ask for its own deadline.
const DefaultExecTimeout = 30 * time.Minute

// Options configures a local driver.
type Options struct {
	// Root is the directory instance workspaces are created under.
	Root string
	// Total is the capacity the driver pretends the machine has, so that
	// scheduler behaviour can be exercised with small, predictable numbers.
	Total vm.Resources
	// MaxInstances optionally caps instance count independently of resources.
	MaxInstances int
	// ExecTimeout overrides DefaultExecTimeout.
	ExecTimeout time.Duration
}

// Driver is a vm.Driver backed by the local filesystem.
type Driver struct {
	opts Options

	mu        sync.Mutex
	instances map[string]*vm.Instance
}

// New returns a local driver rooted at opts.Root, creating it if needed.
func New(opts Options) (*Driver, error) {
	if opts.Root == "" {
		return nil, errors.New("local: a root directory is required")
	}
	if opts.Total.VCPUs <= 0 || opts.Total.MemoryMiB <= 0 || opts.Total.DiskGiB <= 0 {
		return nil, errors.New("local: total capacity must be positive on every axis")
	}
	if opts.ExecTimeout <= 0 {
		opts.ExecTimeout = DefaultExecTimeout
	}
	if err := os.MkdirAll(opts.Root, 0o750); err != nil {
		return nil, fmt.Errorf("local: create root %s: %w", opts.Root, err)
	}
	return &Driver{opts: opts, instances: map[string]*vm.Instance{}}, nil
}

// Name implements vm.Driver.
func (d *Driver) Name() string { return "local" }

// Capacity implements vm.Driver.
func (d *Driver) Capacity(context.Context) (vm.Capacity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.capacityLocked(), nil
}

// capacityLocked totals the resources claimed by live instances. Stopped
// instances release their claim; failed ones never held one.
func (d *Driver) capacityLocked() vm.Capacity {
	cap := vm.Capacity{Total: d.opts.Total, MaxInstances: d.opts.MaxInstances}
	for _, inst := range d.instances {
		if inst.State == vm.StateRunning || inst.State == vm.StatePending {
			cap.Used = cap.Used.Add(inst.Spec.Resources)
			cap.Instances++
		}
	}
	return cap
}

// Create implements vm.Driver.
func (d *Driver) Create(_ context.Context, spec vm.Spec) (*vm.Instance, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.capacityLocked().CanFit(spec.Resources) {
		return nil, fmt.Errorf("local: cannot fit %s: %w", spec.Resources, vm.ErrNoCapacity)
	}

	instanceID := id.New(id.Instance)
	workspace := filepath.Join(d.opts.Root, instanceID, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		return nil, fmt.Errorf("local: create workspace for %s: %w", spec.Name, err)
	}

	inst := &vm.Instance{
		ID:        instanceID,
		ForkID:    spec.ForkID,
		Name:      spec.Name,
		State:     vm.StateRunning,
		Spec:      spec,
		Address:   "127.0.0.1",
		Workspace: workspace,
		CreatedAt: time.Now().UTC(),
	}
	d.instances[instanceID] = inst
	return cloneInstance(inst), nil
}

// Get implements vm.Driver.
func (d *Driver) Get(_ context.Context, instanceID string) (*vm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	inst, ok := d.instances[instanceID]
	if !ok {
		return nil, fmt.Errorf("local: %s: %w", instanceID, vm.ErrNotFound)
	}
	return cloneInstance(inst), nil
}

// List implements vm.Driver.
func (d *Driver) List(context.Context) ([]*vm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*vm.Instance, 0, len(d.instances))
	for _, inst := range d.instances {
		out = append(out, cloneInstance(inst))
	}
	return out, nil
}

// Destroy implements vm.Driver, removing the instance's workspace.
func (d *Driver) Destroy(_ context.Context, instanceID string) error {
	d.mu.Lock()
	inst, ok := d.instances[instanceID]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("local: %s: %w", instanceID, vm.ErrNotFound)
	}
	delete(d.instances, instanceID)
	d.mu.Unlock()

	root := filepath.Dir(inst.Workspace)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("local: remove workspace %s: %w", root, err)
	}
	return nil
}

// Exec implements vm.Driver by running the command on the host, rooted in the
// instance's workspace.
func (d *Driver) Exec(ctx context.Context, instanceID string, cmd vm.Command) (*vm.ExecResult, error) {
	if len(cmd.Argv) == 0 {
		return nil, errors.New("local: exec requires a command")
	}

	d.mu.Lock()
	inst, ok := d.instances[instanceID]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("local: %s: %w", instanceID, vm.ErrNotFound)
	}
	if inst.State != vm.StateRunning {
		return nil, fmt.Errorf("local: %s is %s, not running", instanceID, inst.State)
	}

	timeout := cmd.Timeout
	if timeout <= 0 {
		timeout = d.opts.ExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := cmd.Dir
	if dir == "" {
		dir = inst.Workspace
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(inst.Workspace, dir)
	}

	proc := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...) //nolint:gosec // the caller supplies the argv by design
	proc.Dir = dir
	proc.Env = mergeEnv(inst.Spec.Env, cmd.Env)
	if len(cmd.Stdin) > 0 {
		proc.Stdin = bytes.NewReader(cmd.Stdin)
	}

	var stdout, stderr bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stderr

	started := time.Now()
	err := proc.Run()
	result := &vm.ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(started),
	}

	switch {
	case err == nil:
		result.ExitCode = 0
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.TimedOut = true
		result.ExitCode = -1
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			// The command could not be started at all, which is a driver-level
			// failure rather than a non-zero exit from the workload.
			return nil, fmt.Errorf("local: run %q in %s: %w", cmd.Argv[0], instanceID, err)
		}
	}
	return result, nil
}

// mergeEnv layers command environment over instance environment. The host
// environment is deliberately not inherited: an instance should see only what
// it was given, mirroring how the real driver injects secrets.
func mergeEnv(base, overlay map[string]string) []string {
	merged := make(map[string]string, len(base)+len(overlay)+2)
	// A usable PATH and HOME are the minimum a toolchain needs to run.
	merged["PATH"] = os.Getenv("PATH")
	merged["HOME"] = os.Getenv("HOME")
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overlay {
		merged[k] = v
	}

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// cloneInstance returns a deep copy so callers cannot mutate driver state.
func cloneInstance(in *vm.Instance) *vm.Instance {
	out := *in
	out.Spec.Env = maps(in.Spec.Env)
	out.Spec.Labels = maps(in.Spec.Labels)
	return &out
}

func maps(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Driver satisfies the vm.Driver contract.
var _ vm.Driver = (*Driver)(nil)
