//go:build !linux

package local

import (
	"context"
	"fmt"

	"github.com/dabbers/devex/internal/vm"
)

// OpenSession implements vm.Interactive. Pseudo-terminal allocation here is
// written against Linux, which is also the only platform Firecracker runs on,
// so the development driver matches rather than carrying a second
// implementation for a platform production never sees.
func (d *Driver) OpenSession(context.Context, string, vm.SessionSpec) (vm.Session, error) {
	return nil, fmt.Errorf("local: interactive sessions require Linux: %w", vm.ErrNotSupported)
}

var _ vm.Interactive = (*Driver)(nil)
