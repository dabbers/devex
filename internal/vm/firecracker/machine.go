package firecracker

import (
	"os/exec"
	"path/filepath"
)

// machine is the host-side state of one guest: everything Create made and
// Destroy has to take away again.
type machine struct {
	id    string
	guest Guest
	// clone is the guest's ZFS clone of the golden snapshot.
	clone string
	// jailDir is the jailer's per-guest directory; the chroot is its "root".
	jailDir string

	cmd *exec.Cmd
	// exited is closed once the Firecracker process has been reaped.
	exited chan struct{}
}

// jailDir is where the jailer builds a guest's chroot. The layout is the
// jailer's, not ours: <base>/<exec-file name>/<id>/root.
func (d *Driver) jailDir(instanceID string) string {
	return filepath.Join(d.cfg.ChrootBase, filepath.Base(d.cfg.FirecrackerBin), instanceID)
}

func (m *machine) root() string { return filepath.Join(m.jailDir, "root") }

// vsockPath is the host end of the guest's vsock, inside its chroot.
func (m *machine) vsockPath() string { return filepath.Join(m.root(), jailVsock) }

// consoleLog is the guest's serial console. It sits beside the chroot rather
// than in it, so the jailed process cannot rewrite its own history.
func (m *machine) consoleLog() string { return filepath.Join(m.jailDir, "console.log") }

// Paths as Firecracker sees them from inside the chroot.
const (
	jailKernel = "/vmlinux"
	jailRootfs = "/rootfs"
	jailConfig = "/config.json"
	jailVsock  = "/v.sock"
	// guestCID is the guest's vsock address. Each guest has its own vsock
	// device and its own host socket, so every guest can use the same one.
	guestCID = 3
)
