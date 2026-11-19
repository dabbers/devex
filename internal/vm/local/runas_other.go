//go:build !linux

package local

import (
	"fmt"
	"syscall"

	"github.com/dabbers/devex/internal/vm"
)

// supportsRunAs refuses rather than ignoring the option: a driver asked to
// drop privileges that silently did not would be worse than one that said so.
func supportsRunAs() error {
	return fmt.Errorf("local: running instances as another account requires Linux: %w", vm.ErrNotSupported)
}

func (d *Driver) credential(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }
