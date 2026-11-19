//go:build !linux

package firecracker

import (
	"context"
	"fmt"

	"github.com/dabbers/devex/internal/vm"
)

func (d *Driver) create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, fmt.Errorf("firecracker: booting guests requires Linux and KVM: %w", vm.ErrNotSupported)
}

func (d *Driver) teardown(context.Context, *machine) error { return nil }
