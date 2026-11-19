//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dabbers/devex/internal/vm"
)

func (d *Driver) create(ctx context.Context, spec vm.Spec) (*vm.Instance, error) {
	id, err := newInstanceID()
	if err != nil {
		return nil, err
	}

	// Admission and the pending record happen under one lock, so two Creates
	// racing for the last capacity cannot both get it.
	d.mu.Lock()
	capacity := vm.Capacity{Total: d.cfg.Total, MaxInstances: d.cfg.MaxInstances}
	for _, inst := range d.instances {
		if inst.State == vm.StateRunning || inst.State == vm.StatePending {
			capacity.Used = capacity.Used.Add(inst.Spec.Resources)
			capacity.Instances++
		}
	}
	if !capacity.CanFit(spec.Resources) {
		d.mu.Unlock()
		return nil, fmt.Errorf("firecracker: %s does not fit in %s free: %w", spec.Resources, capacity.Free(), vm.ErrNoCapacity)
	}
	inst := &vm.Instance{
		ID:        id,
		ForkID:    spec.ForkID,
		Name:      spec.Name,
		State:     vm.StatePending,
		Spec:      spec,
		Workspace: Workspace,
		CreatedAt: time.Now().UTC(),
	}
	m := &machine{id: id, jailDir: d.jailDir(id), clone: d.cfg.CloneDataset + "/" + id, exited: make(chan struct{})}
	d.instances[id] = inst
	d.machines[id] = m
	d.mu.Unlock()

	if err := d.boot(ctx, inst, m); err != nil {
		// A guest that did not come up is taken apart rather than left as a
		// failed record: nothing could use it, and every piece of it holds
		// something (a slot, a TAP name, a clone) the next Create may want.
		console := tail(m.consoleLog(), 2048)
		if cleanupErr := d.teardown(context.WithoutCancel(ctx), m); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("and cleaning up failed: %w", cleanupErr))
		}
		d.forget(id)
		if console != "" {
			err = fmt.Errorf("%w\nguest console:\n%s", err, console)
		}
		return nil, err
	}

	d.mu.Lock()
	inst.State = vm.StateRunning
	clone := *inst
	d.mu.Unlock()
	return &clone, nil
}

func (d *Driver) boot(ctx context.Context, inst *vm.Instance, m *machine) error {
	guest, err := d.ReserveSlot(m.id)
	if err != nil {
		return err
	}
	m.guest = guest
	d.mu.Lock()
	inst.Address = guest.GuestIP.String()
	d.mu.Unlock()

	device, grown, err := d.cloneRootfs(ctx, m, inst.Spec.Resources.DiskGiB)
	if err != nil {
		return err
	}
	if err := d.buildJail(inst.Spec, m, device); err != nil {
		return err
	}
	if err := d.createTap(ctx, guest); err != nil {
		return err
	}
	if err := d.launch(m); err != nil {
		return err
	}
	if err := d.awaitAgent(ctx, m); err != nil {
		return err
	}
	if !grown {
		return nil
	}
	// A guest asked for more disk than the golden image has got its clone
	// grown before boot; the filesystem is grown here, online, which ext4
	// does without a prior fsck.
	res, err := d.exec(ctx, m.id, vm.Command{Argv: []string{"resize2fs", "/dev/vda"}, Timeout: 2 * time.Minute}, nil)
	if err == nil && !res.OK() {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		return fmt.Errorf("firecracker: grow root filesystem in %s: %w", m.id, err)
	}
	return nil
}

// markRunning admits a guest to Exec as soon as its agent answers. Create's
// own remaining steps (growing the filesystem) run through Exec, so the
// instance has to be running before Create returns it.
func (d *Driver) markRunning(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if inst, ok := d.instances[id]; ok {
		inst.State = vm.StateRunning
	}
}

// cloneRootfs makes the guest's disk: a ZFS clone of the golden snapshot,
// grown to the size asked for. It returns the clone's block device and whether
// it was grown.
func (d *Driver) cloneRootfs(ctx context.Context, m *machine, diskGiB int) (device string, grown bool, err error) {
	if _, err := run(ctx, "zfs", "clone", "-p", d.cfg.GoldenRootfs, m.clone); err != nil {
		return "", false, fmt.Errorf("firecracker: clone golden image: %w", err)
	}
	out, err := run(ctx, "zfs", "get", "-Hp", "-o", "value", "volsize", m.clone)
	if err != nil {
		return "", false, fmt.Errorf("firecracker: read clone size: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return "", false, fmt.Errorf("firecracker: parse clone size %q: %w", out, err)
	}
	if want := int64(diskGiB) << 30; want > size {
		// Growing a clone allocates nothing: the new space is unwritten
		// blocks until the guest writes them.
		if _, err := run(ctx, "zfs", "set", fmt.Sprintf("volsize=%d", want), m.clone); err != nil {
			return "", false, fmt.Errorf("firecracker: grow clone to %d GiB: %w", diskGiB, err)
		}
		grown = true
	}

	// udev publishes the device asynchronously after the clone exists.
	device = filepath.Join("/dev/zvol", m.clone)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(device); err == nil {
			return device, grown, nil
		}
		if time.Now().After(deadline) {
			return "", false, fmt.Errorf("firecracker: %s did not appear", device)
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// buildJail lays out the chroot the jailer will confine Firecracker to. The
// guest's disk goes in as a device node for its own clone and nothing else, so
// a compromised Firecracker process can open no other guest's disk.
func (d *Driver) buildJail(spec vm.Spec, m *machine, device string) error {
	root := m.root()
	uid := jailerBaseUID + m.guest.Slot
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("firecracker: create jail: %w", err)
	}
	if err := linkOrCopy(d.cfg.KernelImage, filepath.Join(root, jailKernel)); err != nil {
		return fmt.Errorf("firecracker: put kernel in jail: %w", err)
	}

	var st unix.Stat_t
	if err := unix.Stat(device, &st); err != nil {
		return fmt.Errorf("firecracker: stat %s: %w", device, err)
	}
	node := filepath.Join(root, jailRootfs)
	if err := unix.Mknod(node, unix.S_IFBLK|0o600, int(st.Rdev)); err != nil {
		return fmt.Errorf("firecracker: create disk node in jail: %w", err)
	}
	if err := os.Chown(node, uid, uid); err != nil {
		return fmt.Errorf("firecracker: hand disk node to guest uid: %w", err)
	}

	cfg, err := d.BuildMachineConfig(spec, m.guest, jailRootfs)
	if err != nil {
		return err
	}
	cfg.BootSource.KernelImagePath = jailKernel
	cfg.Vsock = &Vsock{GuestCID: guestCID, UDSPath: jailVsock}
	configPath := filepath.Join(root, jailConfig)
	if err := d.WriteMachineConfig(configPath, cfg); err != nil {
		return err
	}
	if err := os.Chown(configPath, uid, uid); err != nil {
		return fmt.Errorf("firecracker: hand config to guest uid: %w", err)
	}
	return nil
}

// linkOrCopy puts a read-only file into a jail, sharing it where the
// filesystem allows.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// createTap makes the guest's TAP device, owned by the guest's uid so the
// jailed Firecracker can attach to it, carrying the host end of its /30.
func (d *Driver) createTap(ctx context.Context, guest Guest) error {
	uid := strconv.Itoa(jailerBaseUID + guest.Slot)
	// A TAP left behind by a guest that died with the control plane would
	// otherwise make this slot unusable.
	_ = deleteLink(ctx, guest.TapDevice)
	steps := [][]string{
		{"ip", "tuntap", "add", "dev", guest.TapDevice, "mode", "tap", "user", uid, "group", uid},
		{"ip", "addr", "add", fmt.Sprintf("%s/%d", guest.HostIP, guest.PrefixLen), "dev", guest.TapDevice},
		{"ip", "link", "set", guest.TapDevice, "up"},
	}
	for _, step := range steps {
		if _, err := run(ctx, step[0], step[1:]...); err != nil {
			return fmt.Errorf("firecracker: set up %s: %w", guest.TapDevice, err)
		}
	}
	return nil
}

func deleteLink(ctx context.Context, name string) error {
	_, err := run(ctx, "ip", "link", "del", name)
	if err != nil && strings.Contains(err.Error(), "Cannot find device") {
		return nil
	}
	return err
}

// launch starts the jailer, which execs into Firecracker in place, so the
// process it returns is the VM itself.
func (d *Driver) launch(m *machine) error {
	console, err := os.OpenFile(m.consoleLog(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("firecracker: open console log: %w", err)
	}
	defer console.Close()

	args := d.JailerArgs(m.id, m.guest, jailConfig)
	// The VM must not live and die with the request that created it.
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // the binary and its arguments come from the operator's config
	cmd.Stdout, cmd.Stderr = console, console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("firecracker: start jailer: %w", err)
	}
	m.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(m.exited)
	}()
	return nil
}

// awaitAgent waits until the guest's agent answers, the VM dies, or the boot
// timeout passes.
func (d *Driver) awaitAgent(ctx context.Context, m *machine) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.BootTimeout)
	defer cancel()
	var last error
	for {
		attempt, stop := context.WithTimeout(ctx, time.Second)
		last = ping(attempt, m.vsockPath())
		stop()
		if last == nil {
			d.markRunning(m.id)
			return nil
		}
		select {
		case <-m.exited:
			return fmt.Errorf("firecracker: %s exited during boot (%v)", m.id, m.cmd.ProcessState)
		case <-ctx.Done():
			return fmt.Errorf("firecracker: %s agent did not answer within %s: %w", m.id, d.cfg.BootTimeout, last)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// teardown removes everything Create made, in reverse, and tolerates any of it
// already being gone, so it serves a half-built guest and a repeated Destroy
// alike.
func (d *Driver) teardown(ctx context.Context, m *machine) error {
	var errs []error
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		select {
		case <-m.exited:
		case <-time.After(10 * time.Second):
			errs = append(errs, fmt.Errorf("firecracker: %s did not exit after SIGKILL", m.id))
		}
	}
	if m.guest.TapDevice != "" {
		if err := deleteLink(ctx, m.guest.TapDevice); err != nil {
			errs = append(errs, fmt.Errorf("firecracker: remove %s: %w", m.guest.TapDevice, err))
		}
	}
	if err := destroyDataset(ctx, m.clone); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(m.jailDir); err != nil {
		errs = append(errs, fmt.Errorf("firecracker: remove jail: %w", err))
	}
	return errors.Join(errs...)
}

// destroyDataset destroys a clone, retrying while the kernel still holds the
// device the VM had open a moment ago.
func destroyDataset(ctx context.Context, dataset string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := run(ctx, "zfs", "destroy", dataset)
		if err == nil || strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		if !strings.Contains(err.Error(), "busy") || time.Now().After(deadline) {
			return fmt.Errorf("firecracker: destroy %s: %w", dataset, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func newInstanceID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("firecracker: instance id: %w", err)
	}
	// The jailer accepts only alphanumerics and hyphens, and the id is also a
	// ZFS dataset name and part of a path.
	return "fc-" + hex.EncodeToString(raw), nil
}

// tail returns the last n bytes of a file, for error reports.
func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > n {
		_, _ = f.Seek(info.Size()-n, io.SeekStart)
	}
	raw, _ := io.ReadAll(f)
	return strings.TrimSpace(string(raw))
}
