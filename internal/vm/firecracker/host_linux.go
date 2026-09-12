//go:build linux

package firecracker

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/dabbers/devex/internal/vm"
)

// probeHost reports what the machine physically has, before the host reserve
// is deducted.
func probeHost() (vm.Resources, error) {
	memMiB, err := totalMemoryMiB("/proc/meminfo")
	if err != nil {
		return vm.Resources{}, err
	}
	diskGiB, err := totalDiskGiB("/")
	if err != nil {
		return vm.Resources{}, err
	}
	return vm.Resources{
		VCPUs:     runtime.NumCPU(),
		MemoryMiB: memMiB,
		DiskGiB:   diskGiB,
	}, nil
}

// totalMemoryMiB reads MemTotal out of a meminfo-formatted file.
func totalMemoryMiB(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("firecracker: read %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, fmt.Errorf("firecracker: malformed MemTotal line %q", line)
		}
		kb, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, fmt.Errorf("firecracker: parse MemTotal %q: %w", fields[0], err)
		}
		return kb / 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("firecracker: scan %s: %w", path, err)
	}
	return 0, fmt.Errorf("firecracker: %s has no MemTotal line", path)
}

// totalDiskGiB reports the size of the filesystem holding path.
func totalDiskGiB(path string) (int, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("firecracker: statfs %s: %w", path, err)
	}
	bytes := uint64(stat.Blocks) * uint64(stat.Bsize) //nolint:unconvert // field widths vary by architecture
	return int(bytes / (1 << 30)), nil
}
