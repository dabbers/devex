// Package vm defines the isolation layer dabberz runs coding agents inside.
//
// Every fork gets its own machine with no mutable state shared with its
// siblings, so agents cannot step on each other's processes or working trees.
// The production driver is Firecracker: agents need full access to their own
// environment and need to drive a real browser, which containers do not give
// cleanly. The interface here is deliberately small so that a local driver can
// stand in during development on a machine without KVM.
package vm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors every driver shares.
var (
	// ErrNotFound reports an unknown instance id.
	ErrNotFound = errors.New("vm: instance not found")
	// ErrNoCapacity reports that the machine cannot fit another instance.
	// Callers queue and wait rather than evicting: dabberz never pre-empts a
	// running fork.
	ErrNoCapacity = errors.New("vm: insufficient capacity")
	// ErrNotSupported reports an operation a driver cannot perform.
	ErrNotSupported = errors.New("vm: operation not supported by this driver")
)

// State is the lifecycle position of a single instance.
type State string

// Instance states.
const (
	StatePending State = "pending"
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// Resources is an allocation of machine capacity.
type Resources struct {
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memory_mib"`
	DiskGiB   int `json:"disk_gib"`
}

// Add returns the sum of two allocations.
func (r Resources) Add(other Resources) Resources {
	return Resources{
		VCPUs:     r.VCPUs + other.VCPUs,
		MemoryMiB: r.MemoryMiB + other.MemoryMiB,
		DiskGiB:   r.DiskGiB + other.DiskGiB,
	}
}

// Sub returns r reduced by other, clamped at zero on each axis.
func (r Resources) Sub(other Resources) Resources {
	return Resources{
		VCPUs:     max(r.VCPUs-other.VCPUs, 0),
		MemoryMiB: max(r.MemoryMiB-other.MemoryMiB, 0),
		DiskGiB:   max(r.DiskGiB-other.DiskGiB, 0),
	}
}

// Fits reports whether an allocation of want fits within r on every axis.
func (r Resources) Fits(want Resources) bool {
	return want.VCPUs <= r.VCPUs && want.MemoryMiB <= r.MemoryMiB && want.DiskGiB <= r.DiskGiB
}

// String implements fmt.Stringer.
func (r Resources) String() string {
	return fmt.Sprintf("%dvcpu/%dMiB/%dGiB", r.VCPUs, r.MemoryMiB, r.DiskGiB)
}

// Capacity is what the machine has and what is currently claimed.
type Capacity struct {
	Total Resources `json:"total"`
	Used  Resources `json:"used"`
	// Instances is the number of live instances counted in Used.
	Instances int `json:"instances"`
	// MaxInstances caps instance count independently of resources; zero means
	// only the resource totals apply.
	MaxInstances int `json:"max_instances"`
}

// Free reports the unclaimed capacity.
func (c Capacity) Free() Resources { return c.Total.Sub(c.Used) }

// CanFit reports whether another instance of the given size can be admitted.
func (c Capacity) CanFit(want Resources) bool {
	if c.MaxInstances > 0 && c.Instances >= c.MaxInstances {
		return false
	}
	return c.Free().Fits(want)
}

// Spec describes an instance to create.
type Spec struct {
	// ForkID ties the instance back to the workstream it serves. It is empty
	// for shared infrastructure such as the verifier's UI VM.
	ForkID string `json:"fork_id,omitempty"`
	// Name is a short human-readable label, unique per driver.
	Name      string    `json:"name"`
	Resources Resources `json:"resources"`
	// Image names the golden image to boot. v1 provisions a single generic
	// image carrying the full toolchain rather than one image per toolchain.
	Image string `json:"image"`
	// Env is injected into the instance, and carries the repo-scoped secrets
	// the fork inherits.
	Env map[string]string `json:"env,omitempty"`
	// Labels are free-form metadata for operators and for reconciliation after
	// a control-plane restart.
	Labels map[string]string `json:"labels,omitempty"`
}

// Validate checks a spec for the fields every driver requires.
func (s Spec) Validate() error {
	if s.Name == "" {
		return errors.New("vm: spec requires a name")
	}
	if s.Resources.VCPUs <= 0 {
		return fmt.Errorf("vm: spec %q requires at least one vcpu", s.Name)
	}
	if s.Resources.MemoryMiB <= 0 {
		return fmt.Errorf("vm: spec %q requires a positive memory size", s.Name)
	}
	if s.Resources.DiskGiB <= 0 {
		return fmt.Errorf("vm: spec %q requires a positive disk size", s.Name)
	}
	if s.Image == "" {
		return fmt.Errorf("vm: spec %q requires an image", s.Name)
	}
	return nil
}

// Instance is a running (or failed) machine.
type Instance struct {
	ID     string `json:"id"`
	ForkID string `json:"fork_id,omitempty"`
	Name   string `json:"name"`
	State  State  `json:"state"`
	Spec   Spec   `json:"spec"`
	// Address is where the control plane and the reverse proxy reach the
	// instance. For Firecracker this is the guest's address on the host bridge.
	Address string `json:"address"`
	// SSHPort is the host port forwarded to the guest's SSH daemon, backing the
	// workspace's shell and SSH access.
	SSHPort int `json:"ssh_port,omitempty"`
	// Workspace is the path, inside the instance, where the repo is checked out.
	Workspace string `json:"workspace"`
	// Message carries failure detail when State is StateFailed.
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Command is a single command to run inside an instance.
type Command struct {
	// Argv is the command and its arguments; it is never passed through a
	// shell, so callers needing shell semantics invoke one explicitly.
	Argv []string `json:"argv"`
	// Dir is the working directory inside the instance. Empty means the
	// instance's workspace.
	Dir string            `json:"dir,omitempty"`
	Env map[string]string `json:"env,omitempty"`
	// Stdin is fed to the command, for agent prompts and patches.
	Stdin []byte `json:"-"`
	// Timeout bounds the run. Zero means the driver's default.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ExecResult is the outcome of a Command.
type ExecResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	Duration time.Duration `json:"duration"`
	// TimedOut reports that the command was killed at its deadline rather than
	// exiting on its own.
	TimedOut bool `json:"timed_out"`
}

// OK reports whether the command exited cleanly.
func (r *ExecResult) OK() bool { return r != nil && r.ExitCode == 0 && !r.TimedOut }

// Driver creates and supervises instances on one machine.
//
// Implementations must be safe for concurrent use: the orchestrator drives
// many forks at once.
type Driver interface {
	// Name identifies the driver in logs and diagnostics.
	Name() string
	// Capacity reports total and claimed machine resources.
	Capacity(ctx context.Context) (Capacity, error)
	// Create boots an instance. It returns ErrNoCapacity rather than evicting
	// anything when the machine is full.
	Create(ctx context.Context, spec Spec) (*Instance, error)
	// Get returns one instance by id, or ErrNotFound.
	Get(ctx context.Context, instanceID string) (*Instance, error)
	// List returns every instance the driver knows about, including stopped
	// ones: dabberz never reclaims instances automatically, so operators need
	// to see what is still allocated.
	List(ctx context.Context) ([]*Instance, error)
	// Destroy tears an instance down. It is only ever called by explicit
	// manual cleanup, never by the lifecycle itself.
	Destroy(ctx context.Context, instanceID string) error
	// Exec runs a command inside an instance and waits for it to finish.
	Exec(ctx context.Context, instanceID string, cmd Command) (*ExecResult, error)
}
