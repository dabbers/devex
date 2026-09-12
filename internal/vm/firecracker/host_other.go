//go:build !linux

package firecracker

import (
	"errors"

	"github.com/dabbers/devex/internal/vm"
)

// probeHost is unavailable off Linux. Firecracker requires KVM, so this path
// only matters for building and testing the control plane on a developer
// machine, where the capacity totals must be configured explicitly.
func probeHost() (vm.Resources, error) {
	return vm.Resources{}, errors.New("firecracker: host capacity probing requires Linux; set total capacity explicitly")
}
