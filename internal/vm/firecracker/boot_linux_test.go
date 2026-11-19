//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/vm"
)

// These tests boot real guests. They need root, /dev/kvm, and a host prepared
// as docs/HOST.md describes; they skip unless all of it is there:
//
//	DABBERZ_FC_TEST_BIN     directory holding firecracker and jailer
//	DABBERZ_FC_TEST_KERNEL  guest kernel (uncompressed vmlinux)
//	DABBERZ_FC_TEST_GOLDEN  golden snapshot, e.g. tank/dabberz/fc-dev/golden@base
//	DABBERZ_FC_TEST_JAIL    chroot base directory; on the same filesystem as
//	                        the kernel so the kernel is linked, not copied
//	DABBERZ_FC_TEST_CLONES  dataset for per-guest clones
//	DABBERZ_FC_TEST_NETWORK optional; the CIDR guests are numbered from
func bootDriver(t *testing.T) *Driver {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("booting guests needs root")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("booting guests needs /dev/kvm")
	}
	env := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Skipf("%s is not set", name)
		}
		return value
	}
	bin := env("DABBERZ_FC_TEST_BIN")
	cfg := Config{
		FirecrackerBin: filepath.Join(bin, "firecracker"),
		JailerBin:      filepath.Join(bin, "jailer"),
		KernelImage:    env("DABBERZ_FC_TEST_KERNEL"),
		GoldenRootfs:   env("DABBERZ_FC_TEST_GOLDEN"),
		ChrootBase:     env("DABBERZ_FC_TEST_JAIL"),
		CloneDataset:   env("DABBERZ_FC_TEST_CLONES"),
		Network:        os.Getenv("DABBERZ_FC_TEST_NETWORK"),
		BootTimeout:    30 * time.Second,
		Total:          vm.Resources{VCPUs: 8, MemoryMiB: 8192, DiskGiB: 100},
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func smallSpec(name string) vm.Spec {
	return vm.Spec{
		Name:      name,
		Image:     "golden",
		Resources: vm.Resources{VCPUs: 1, MemoryMiB: 512, DiskGiB: 1},
		Env:       map[string]string{"DABBERZ_TEST_SPEC_ENV": "from-spec"},
	}
}

// boot creates a guest and destroys it when the test ends, whatever happens.
func boot(t *testing.T, d *Driver, spec vm.Spec) *vm.Instance {
	t.Helper()
	inst, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Destroy(context.Background(), inst.ID); err != nil && !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Destroy %s: %v", inst.ID, err)
		}
	})
	return inst
}

func mustExec(t *testing.T, d *Driver, id string, argv ...string) *vm.ExecResult {
	t.Helper()
	res, err := d.Exec(context.Background(), id, vm.Command{Argv: argv, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Exec %v: %v", argv, err)
	}
	if !res.OK() {
		t.Fatalf("Exec %v: exit %d timedOut=%v stderr=%q", argv, res.ExitCode, res.TimedOut, res.Stderr)
	}
	return res
}

func TestBootTimeToFirstExec(t *testing.T) {
	d := bootDriver(t)
	started := time.Now()
	inst := boot(t, d, smallSpec("first-exec"))
	booted := time.Since(started)
	res := mustExec(t, d, inst.ID, "true")
	first := time.Since(started)
	t.Logf("time to first exec: %s (Create %s, first Exec %s)", first, booted, res.Duration)
	// The copy-on-write rule exists so that a fresh guest is usable in
	// seconds. A boot that has crept past this has lost the point of it.
	if first > 15*time.Second {
		t.Errorf("time to first exec %s, want well under 15s", first)
	}
}

func TestExecRunsInTheGuest(t *testing.T) {
	d := bootDriver(t)
	inst := boot(t, d, smallSpec("exec"))
	ctx := context.Background()

	// It is the guest's kernel answering, not the host's.
	hostKernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustExec(t, d, inst.ID, "uname", "-r").Stdout; strings.TrimSpace(got) == strings.TrimSpace(string(hostKernel)) {
		t.Fatalf("uname -r = %q: the command ran on the host kernel", got)
	}

	if got := mustExec(t, d, inst.ID, "pwd").Stdout; strings.TrimSpace(got) != Workspace {
		t.Errorf("default directory = %q, want %s", got, Workspace)
	}
	if got := mustExec(t, d, inst.ID, "sh", "-c", "echo $DABBERZ_TEST_SPEC_ENV $X").Stdout; strings.TrimSpace(got) != "from-spec" {
		t.Errorf("spec env not passed: %q", got)
	}
	res, err := d.Exec(ctx, inst.ID, vm.Command{
		Argv:  []string{"sh", "-c", "cat; echo oops >&2; exit 7"},
		Stdin: []byte("fed through stdin"),
		Env:   map[string]string{"X": "y"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 || res.Stdout != "fed through stdin" || strings.TrimSpace(res.Stderr) != "oops" {
		t.Errorf("result = exit %d stdout %q stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// A command that cannot start is a driver error, as with the local driver.
	if _, err := d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"/no/such/binary"}}); err == nil {
		t.Error("Exec of a missing binary reported no error")
	}

	// The deadline is enforced in the guest and reported as a timeout.
	started := time.Now()
	// The sleep is a child of the shell, not the process the agent started,
	// so only killing the whole group stops it.
	res, err = d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c", "sleep 60; echo never"}, Timeout: time.Second})
	if err != nil {
		t.Fatalf("Exec with timeout: %v", err)
	}
	if !res.TimedOut || time.Since(started) > 10*time.Second {
		t.Errorf("timeout not enforced: %+v after %s", res, time.Since(started))
	}
	// ... and the process group is gone afterwards, not left running.
	if out := mustExec(t, d, inst.ID, "sh", "-c", "pgrep -x sleep || true").Stdout; strings.TrimSpace(out) != "" {
		t.Errorf("timed-out command still running: pids %q", out)
	}

	var lines []string
	res, err = d.ExecStream(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c", "echo one; sleep 0.2; echo two; printf three"}}, func(line []byte) {
		lines = append(lines, string(line))
	})
	if err != nil || !res.OK() {
		t.Fatalf("ExecStream: %v %+v", err, res)
	}
	if strings.Join(lines, ",") != "one,two,three" {
		t.Errorf("streamed lines = %q", lines)
	}
}

func TestGuestNetworkFollowsTheLayout(t *testing.T) {
	d := bootDriver(t)
	inst := boot(t, d, smallSpec("net"))
	guest, err := d.ReserveSlot(inst.ID) // already held: returns its layout
	if err != nil {
		t.Fatal(err)
	}
	if inst.Address != guest.GuestIP.String() {
		t.Errorf("instance address %s, layout says %s", inst.Address, guest.GuestIP)
	}
	addr := mustExec(t, d, inst.ID, "ip", "-4", "-o", "addr", "show", "eth0").Stdout
	if !strings.Contains(addr, guest.GuestIP.String()+"/30") {
		t.Errorf("eth0 = %q, want %s/30", addr, guest.GuestIP)
	}
	mustExec(t, d, inst.ID, "ping", "-c", "1", "-W", "2", guest.HostIP.String())
}

func TestGuestsWriteToTheirOwnClone(t *testing.T) {
	d := bootDriver(t)
	first := boot(t, d, smallSpec("writer"))
	mustExec(t, d, first.ID, "sh", "-c", "echo mine > /etc/written-by-guest && sync")

	// The disk is a clone of the golden snapshot, not a copy and not the
	// golden image itself.
	origin, err := run(context.Background(), "zfs", "get", "-H", "-o", "value", "origin", d.machines[first.ID].clone)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(origin) != d.cfg.GoldenRootfs {
		t.Errorf("guest disk origin = %q, want %s", origin, d.cfg.GoldenRootfs)
	}

	second := boot(t, d, smallSpec("reader"))
	res, err := d.Exec(context.Background(), second.ID, vm.Command{Argv: []string{"test", "-e", "/etc/written-by-guest"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a second guest sees the first guest's write")
	}
}

func TestDiskIsGrownToTheSpec(t *testing.T) {
	d := bootDriver(t)
	spec := smallSpec("big")
	spec.Resources.DiskGiB = 8
	inst := boot(t, d, spec)
	out := mustExec(t, d, inst.ID, "df", "--output=size", "-B1G", "/").Stdout
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("df: %q", out)
	}
	var gib int
	fmt.Sscan(fields[len(fields)-1], &gib)
	if gib < 7 {
		t.Errorf("root filesystem is %d GiB, want about %d", gib, spec.Resources.DiskGiB)
	}
}

func TestGuestIsJailed(t *testing.T) {
	d := bootDriver(t)
	inst := boot(t, d, smallSpec("jailed"))
	m := d.machines[inst.ID]
	pid := m.cmd.Process.Pid

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(jailerBaseUID + m.guest.Slot)
	for _, line := range strings.Split(string(status), "\n") {
		if field, ok := strings.CutPrefix(line, "Uid:"); ok {
			for _, uid := range strings.Fields(field) {
				if uid != want {
					t.Errorf("firecracker runs with uid %s, want %s on every id: %q", uid, want, line)
				}
			}
		}
	}
	// The jailer pivots into the chroot inside a mount namespace of its own,
	// so the link reads "/" from out here; what identifies the jail is that
	// the process's root is the jail directory's inode.
	procRoot, err := os.Stat(fmt.Sprintf("/proc/%d/root/", pid))
	if err != nil {
		t.Fatal(err)
	}
	jailRoot, err := os.Stat(m.root())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(procRoot, jailRoot) {
		t.Errorf("firecracker's root is not its jail %s", m.root())
	}
	ownNS, _ := os.Readlink("/proc/self/ns/mnt")
	if theirs, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", pid)); theirs == ownNS {
		t.Errorf("firecracker shares the host's mount namespace (%s)", theirs)
	}
	// The only disk in the jail is this guest's own.
	var disks []string
	_ = filepath.Walk(m.root(), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
			disks = append(disks, path)
		}
		return nil
	})
	if len(disks) != 1 || disks[0] != filepath.Join(m.root(), jailRootfs) {
		t.Errorf("block devices in the jail: %v", disks)
	}
	// The kernel is shared with every jail; handing it to one guest's uid
	// would let that guest rewrite what every later guest boots.
	if info, err := os.Stat(d.cfg.KernelImage); err == nil {
		if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 {
			t.Errorf("shared kernel is owned by uid %d", st.Uid)
		}
	}
}

func TestDestroyRemovesEverything(t *testing.T) {
	d := bootDriver(t)
	ctx := context.Background()
	inst, err := d.Create(ctx, smallSpec("doomed"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := d.machines[inst.ID]
	pid := m.cmd.Process.Pid

	if err := d.Destroy(ctx, inst.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	assertGone(t, m, pid)
	if err := d.Destroy(ctx, inst.ID); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("second Destroy = %v, want ErrNotFound", err)
	}
	if _, err := d.Get(ctx, inst.ID); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get after Destroy = %v, want ErrNotFound", err)
	}
	// The slot is free again, so the next guest reuses its addresses.
	if c, _ := d.Capacity(ctx); c.Instances != 0 {
		t.Errorf("capacity still counts %d instances", c.Instances)
	}
}

func TestFailedBootLeavesNothingBehind(t *testing.T) {
	d := bootDriver(t)
	// A kernel that is not a kernel: everything up to the boot succeeds, then
	// Firecracker refuses it.
	bogus := filepath.Join(d.cfg.ChrootBase, "not-a-kernel")
	if err := os.WriteFile(bogus, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(bogus) })
	d.cfg.KernelImage = bogus

	before := clones(t, d)
	_, err := d.Create(context.Background(), smallSpec("broken"))
	if err == nil {
		t.Fatal("Create with a bogus kernel succeeded")
	}
	t.Logf("Create failed as expected: %v", firstLine(err.Error()))
	if after := clones(t, d); after != before {
		t.Errorf("clones before %q, after %q", before, after)
	}
	if entries, _ := os.ReadDir(filepath.Join(d.cfg.ChrootBase, "firecracker")); len(entries) != 0 {
		t.Errorf("jail directories left behind: %v", entries)
	}
	if out, _ := exec.Command("ip", "-o", "link", "show", "type", "tun").Output(); bytes.Contains(out, []byte("fc-tap")) {
		t.Errorf("TAP devices left behind: %s", out)
	}
	if c, _ := d.Capacity(context.Background()); c.Instances != 0 {
		t.Errorf("a failed Create still holds capacity: %+v", c)
	}
}

func TestSessionIsATerminalInTheGuest(t *testing.T) {
	d := bootDriver(t)
	inst := boot(t, d, smallSpec("session"))
	s, err := d.OpenSession(context.Background(), inst.ID, vm.SessionSpec{Argv: []string{"/bin/sh"}, Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer s.Close()

	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := s.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	if _, err := s.Write([]byte("tty; stty size; exit 3\n")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var code int
	go func() { code, err = s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("session did not end")
	}
	mu.Lock()
	text := out.String()
	mu.Unlock()
	if code != 3 {
		t.Errorf("session exit = %d (%v), want 3; output %q", code, err, text)
	}
	if !strings.Contains(text, "/dev/pts/") || !strings.Contains(text, "30 100") {
		t.Errorf("session output %q lacks a terminal or its size", text)
	}
}

func clones(t *testing.T, d *Driver) string {
	t.Helper()
	out, err := run(context.Background(), "zfs", "list", "-H", "-o", "name", "-r", d.cfg.CloneDataset)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertGone(t *testing.T, m *machine, pid int) {
	t.Helper()
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		t.Errorf("firecracker pid %d is still running", pid)
	}
	if _, err := os.Stat(filepath.Join("/sys/class/net", m.guest.TapDevice)); err == nil {
		t.Errorf("TAP %s still exists", m.guest.TapDevice)
	}
	if _, err := run(context.Background(), "zfs", "list", m.clone); err == nil {
		t.Errorf("clone %s still exists", m.clone)
	}
	if _, err := os.Stat(m.jailDir); err == nil {
		t.Errorf("jail %s still exists", m.jailDir)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
