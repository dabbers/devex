package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/vm"
)

func testConfig() Config {
	return Config{
		KernelImage:  "/srv/dabberz/vmlinux",
		GoldenRootfs: "/srv/dabberz/golden.ext4",
		ChrootBase:   "/srv/dabberz/vm",
		Network:      "172.30.0.0/16",
		Total:        vm.Resources{VCPUs: 64, MemoryMiB: 262144, DiskGiB: 2000},
	}
}

func newDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestConfigValidation(t *testing.T) {
	tests := map[string]func(*Config){
		"missing kernel": func(c *Config) { c.KernelImage = "" },
		"missing rootfs": func(c *Config) { c.GoldenRootfs = "" },
		"bad network":    func(c *Config) { c.Network = "not-a-cidr" },
		"bad ssh base":   func(c *Config) { c.SSHPortBase = 80 },
	}
	for name, mutate := range tests {
		cfg := testConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want an error", name)
		}
	}
	if err := testConfig().Validate(); err != nil {
		t.Errorf("Validate(valid config) = %v", err)
	}
}

func TestLayoutGivesEachGuestItsOwnSubnet(t *testing.T) {
	d := newDriver(t)

	first, err := d.Layout(0)
	if err != nil {
		t.Fatalf("Layout(0): %v", err)
	}
	if first.HostIP.String() != "172.30.0.1" || first.GuestIP.String() != "172.30.0.2" {
		t.Fatalf("slot 0 addresses = %s/%s, want 172.30.0.1/172.30.0.2", first.HostIP, first.GuestIP)
	}
	if first.PrefixLen != 30 {
		t.Fatalf("prefix = /%d, want /30", first.PrefixLen)
	}

	second, err := d.Layout(1)
	if err != nil {
		t.Fatalf("Layout(1): %v", err)
	}
	if second.HostIP.String() != "172.30.0.5" || second.GuestIP.String() != "172.30.0.6" {
		t.Fatalf("slot 1 addresses = %s/%s, want 172.30.0.5/172.30.0.6", second.HostIP, second.GuestIP)
	}

	// Distinct guests must not collide on any host-side resource.
	if first.TapDevice == second.TapDevice {
		t.Error("two guests share a TAP device")
	}
	if first.SSHPort == second.SSHPort {
		t.Error("two guests share an SSH port")
	}
	if first.ChrootDir == second.ChrootDir {
		t.Error("two guests share a chroot")
	}
	if first.MAC == second.MAC {
		t.Error("two guests share a MAC address")
	}
}

func TestLayoutSubnetsNeverOverlap(t *testing.T) {
	d := newDriver(t)
	seen := map[string]int{}
	for slot := range 500 {
		guest, err := d.Layout(slot)
		if err != nil {
			t.Fatalf("Layout(%d): %v", slot, err)
		}
		for _, addr := range []netip.Addr{guest.HostIP, guest.GuestIP} {
			if prev, dup := seen[addr.String()]; dup {
				t.Fatalf("address %s assigned to both slot %d and slot %d", addr, prev, slot)
			}
			seen[addr.String()] = slot
		}
		if !d.network.Contains(guest.GuestIP) {
			t.Fatalf("slot %d guest %s escaped network %s", slot, guest.GuestIP, d.network)
		}
	}
}

func TestLayoutRejectsSlotsOutsideTheNetwork(t *testing.T) {
	cfg := testConfig()
	// A /29 holds exactly two /30s, so slot 2 must not fit.
	cfg.Network = "192.168.50.0/29"
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Layout(1); err != nil {
		t.Fatalf("Layout(1) should fit in a /29: %v", err)
	}
	if _, err := d.Layout(2); err == nil {
		t.Fatal("Layout(2) should overflow a /29")
	}
	if _, err := d.Layout(-1); err == nil {
		t.Fatal("Layout(-1) should be rejected")
	}
}

func TestReserveSlotIsStableAndReusableAfterRelease(t *testing.T) {
	d := newDriver(t)

	a, err := d.ReserveSlot("vm_a")
	if err != nil {
		t.Fatalf("ReserveSlot(a): %v", err)
	}
	b, err := d.ReserveSlot("vm_b")
	if err != nil {
		t.Fatalf("ReserveSlot(b): %v", err)
	}
	if a.Slot == b.Slot {
		t.Fatal("two instances were given the same slot")
	}

	// Re-reserving is idempotent: a guest keeps its identity.
	again, err := d.ReserveSlot("vm_a")
	if err != nil {
		t.Fatalf("ReserveSlot(a, repeat): %v", err)
	}
	if again.Slot != a.Slot || again.GuestIP != a.GuestIP {
		t.Fatalf("slot for vm_a moved from %d to %d", a.Slot, again.Slot)
	}

	// A released slot is handed to the next guest rather than leaking.
	d.ReleaseSlot("vm_a")
	c, err := d.ReserveSlot("vm_c")
	if err != nil {
		t.Fatalf("ReserveSlot(c): %v", err)
	}
	if c.Slot != a.Slot {
		t.Fatalf("released slot %d was not reused (got %d)", a.Slot, c.Slot)
	}

	if _, err := d.ReserveSlot(""); err == nil {
		t.Fatal("ReserveSlot(\"\") should be rejected")
	}
}

func TestBuildMachineConfigMatchesGuestLayout(t *testing.T) {
	d := newDriver(t)
	guest, err := d.Layout(3)
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	spec := vm.Spec{
		Name:      "fork-ratings",
		Image:     "dabberz-golden",
		Resources: vm.Resources{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40},
	}

	cfg, err := d.BuildMachineConfig(spec, guest, "/srv/dabberz/vm/slot-3/rootfs.ext4")
	if err != nil {
		t.Fatalf("BuildMachineConfig: %v", err)
	}
	if cfg.MachineConfig.VCPUCount != 4 || cfg.MachineConfig.MemSizeMiB != 8192 {
		t.Fatalf("machine sizing not applied: %+v", cfg.MachineConfig)
	}
	if len(cfg.Drives) != 1 || !cfg.Drives[0].IsRootDevice {
		t.Fatalf("expected exactly one root drive, got %+v", cfg.Drives)
	}
	// Guests must boot their own overlay, never the shared golden image.
	if cfg.Drives[0].PathOnHost == d.cfg.GoldenRootfs {
		t.Fatal("guest was pointed at the shared golden image instead of an overlay")
	}
	if cfg.Drives[0].IsReadOnly {
		t.Fatal("the guest root device must be writable")
	}
	if len(cfg.NetworkInterfaces) != 1 || cfg.NetworkInterfaces[0].HostDevName != guest.TapDevice {
		t.Fatalf("network interface does not match the guest layout: %+v", cfg.NetworkInterfaces)
	}
	if cfg.NetworkInterfaces[0].GuestMAC != guest.MAC {
		t.Fatalf("MAC mismatch: %q vs %q", cfg.NetworkInterfaces[0].GuestMAC, guest.MAC)
	}
	// Static addressing on the kernel command line, so the golden image needs no DHCP.
	wantIP := "ip=" + guest.GuestIP.String() + "::" + guest.HostIP.String() + ":255.255.255.252"
	if !strings.Contains(cfg.BootSource.BootArgs, wantIP) {
		t.Fatalf("boot args %q do not carry %q", cfg.BootSource.BootArgs, wantIP)
	}

	if _, err := d.BuildMachineConfig(spec, guest, ""); err == nil {
		t.Fatal("BuildMachineConfig should require an overlay path")
	}
	if _, err := d.BuildMachineConfig(vm.Spec{}, guest, "/x"); err == nil {
		t.Fatal("BuildMachineConfig should validate the spec")
	}
}

func TestWriteMachineConfigEmitsFirecrackerJSON(t *testing.T) {
	d := newDriver(t)
	guest, err := d.Layout(0)
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	spec := vm.Spec{Name: "n", Image: "i", Resources: vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10}}
	cfg, err := d.BuildMachineConfig(spec, guest, "/tmp/rootfs.ext4")
	if err != nil {
		t.Fatalf("BuildMachineConfig: %v", err)
	}

	path := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := d.WriteMachineConfig(path, cfg); err != nil {
		t.Fatalf("WriteMachineConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// The file must use the Firecracker API's own key names to be loadable.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	for _, key := range []string{"boot-source", "drives", "machine-config", "network-interfaces"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("config is missing the %q section", key)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config permissions = %o, want 600", perm)
	}
}

func TestJailerArgsConfineEachGuest(t *testing.T) {
	d := newDriver(t)
	first, _ := d.Layout(0)
	second, _ := d.Layout(1)

	argsFor := func(guest Guest, instanceID string) map[string]string {
		args := d.JailerArgs(instanceID, guest, "/cfg.json")
		flags := map[string]string{}
		for i := 0; i+1 < len(args); i++ {
			if strings.HasPrefix(args[i], "--") {
				flags[args[i]] = args[i+1]
			}
		}
		return flags
	}

	a := argsFor(first, "vm_a")
	b := argsFor(second, "vm_b")
	if a["--uid"] == b["--uid"] {
		t.Error("two guests were jailed under the same uid")
	}
	if a["--id"] == b["--id"] {
		t.Error("two guests were given the same jailer id")
	}
	if a["--chroot-base-dir"] == "" || a["--exec-file"] == "" {
		t.Errorf("jailer invocation is incomplete: %v", a)
	}
}

func TestUnimplementedOperationsReportNotSupported(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	spec := vm.Spec{Name: "n", Image: "i", Resources: vm.Resources{VCPUs: 1, MemoryMiB: 512, DiskGiB: 5}}

	// The boot path is not implemented yet, and must say so plainly rather
	// than silently appearing to succeed.
	if _, err := d.Create(ctx, spec); !errors.Is(err, vm.ErrNotSupported) {
		t.Errorf("Create = %v, want vm.ErrNotSupported", err)
	}
	if err := d.Destroy(ctx, "vm_x"); !errors.Is(err, vm.ErrNotSupported) {
		t.Errorf("Destroy = %v, want vm.ErrNotSupported", err)
	}
	if _, err := d.Exec(ctx, "vm_x", vm.Command{Argv: []string{"true"}}); !errors.Is(err, vm.ErrNotSupported) {
		t.Errorf("Exec = %v, want vm.ErrNotSupported", err)
	}
	// Create must still reject an invalid spec before reaching the stub.
	if _, err := d.Create(ctx, vm.Spec{}); errors.Is(err, vm.ErrNotSupported) {
		t.Error("Create should validate the spec before reporting the boot path unimplemented")
	}
	if _, err := d.Get(ctx, "vm_x"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get = %v, want vm.ErrNotFound", err)
	}
}

func TestCapacityReservesHeadroomForTheControlPlane(t *testing.T) {
	capacity, err := newDriver(t).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if capacity.Total.VCPUs != 64 {
		t.Fatalf("total = %s, want the configured totals", capacity.Total)
	}
	if capacity.Instances != 0 {
		t.Fatalf("instances = %d, want 0 on a fresh driver", capacity.Instances)
	}
}

func TestPrefixMask(t *testing.T) {
	for bits, want := range map[int]string{30: "255.255.255.252", 24: "255.255.255.0", 16: "255.255.0.0", 0: "0.0.0.0"} {
		if got := prefixMask(bits); got != want {
			t.Errorf("prefixMask(%d) = %s, want %s", bits, got, want)
		}
	}
}
