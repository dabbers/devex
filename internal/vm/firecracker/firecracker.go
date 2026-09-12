// Package firecracker implements the production vm.Driver on Firecracker
// microVMs.
//
// Firecracker rather than containers is a requirement, not a preference: an
// agent needs full access to its own environment and needs to drive a real,
// non-headless browser, and a shared kernel gives neither safely. Each fork
// gets its own microVM booted from a single generic golden image carrying the
// whole toolchain, so spin-up does not depend on per-toolchain image builds.
//
// Boot status: the layout, networking, capacity accounting and machine
// configuration in this package are implemented and tested. Actually launching
// the jailer and driving the Firecracker API socket is NOT yet implemented --
// Create returns vm.ErrNotSupported. Until that lands, the control plane runs
// on the local driver.
package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/dabbers/devex/internal/vm"
)

// Defaults for a fresh deployment.
const (
	DefaultBridgeName  = "fcbr0"
	DefaultSSHPortBase = 22000
	// DefaultNetwork gives each microVM a /30 off a private /16, which is the
	// standard Firecracker recipe: one TAP device per guest, host and guest
	// occupying the two usable addresses.
	DefaultNetwork = "172.30.0.0/16"
)

// Config configures the driver against a host.
type Config struct {
	// FirecrackerBin and JailerBin are the binaries used to boot guests. The
	// jailer is what confines each VM to its own chroot, uid and cgroup.
	FirecrackerBin string `yaml:"firecracker_bin" json:"firecracker_bin"`
	JailerBin      string `yaml:"jailer_bin" json:"jailer_bin"`
	// KernelImage is the uncompressed kernel every guest boots.
	KernelImage string `yaml:"kernel_image" json:"kernel_image"`
	// GoldenRootfs is the single generic image carrying the full toolchain.
	// Each VM boots a copy-on-write overlay of it rather than the file itself.
	GoldenRootfs string `yaml:"golden_rootfs" json:"golden_rootfs"`
	// ChrootBase is the directory the jailer builds per-VM chroots under.
	ChrootBase string `yaml:"chroot_base" json:"chroot_base"`
	// BridgeName is the host bridge every TAP device is enslaved to.
	BridgeName string `yaml:"bridge_name" json:"bridge_name"`
	// Network is the CIDR guest /30s are carved from.
	Network string `yaml:"network" json:"network"`
	// SSHPortBase is the first host port forwarded to a guest's SSH daemon.
	SSHPortBase int `yaml:"ssh_port_base" json:"ssh_port_base"`
	// Total caps what the driver will hand out. Left zero, the driver probes
	// the host and reserves HostReserve for the control plane itself.
	Total vm.Resources `yaml:"total" json:"total"`
	// HostReserve is held back from the probed host capacity so that the
	// control plane, the proxy and the shared UI VM are never squeezed out.
	HostReserve vm.Resources `yaml:"host_reserve" json:"host_reserve"`
	// MaxInstances optionally caps guest count independently of resources.
	MaxInstances int `yaml:"max_instances" json:"max_instances"`
}

// withDefaults fills unset fields.
func (c Config) withDefaults() Config {
	if c.FirecrackerBin == "" {
		c.FirecrackerBin = "firecracker"
	}
	if c.JailerBin == "" {
		c.JailerBin = "jailer"
	}
	if c.BridgeName == "" {
		c.BridgeName = DefaultBridgeName
	}
	if c.Network == "" {
		c.Network = DefaultNetwork
	}
	if c.SSHPortBase == 0 {
		c.SSHPortBase = DefaultSSHPortBase
	}
	if c.ChrootBase == "" {
		c.ChrootBase = "/srv/dabberz/vm"
	}
	return c
}

// Validate checks the configuration needed to boot guests.
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.KernelImage == "" {
		return errors.New("firecracker: kernel_image is required")
	}
	if c.GoldenRootfs == "" {
		return errors.New("firecracker: golden_rootfs is required")
	}
	if _, err := netip.ParsePrefix(c.Network); err != nil {
		return fmt.Errorf("firecracker: network %q is not a valid CIDR: %w", c.Network, err)
	}
	if c.SSHPortBase < 1024 || c.SSHPortBase > 65000 {
		return fmt.Errorf("firecracker: ssh_port_base %d is out of range", c.SSHPortBase)
	}
	return nil
}

// Driver is the Firecracker-backed vm.Driver.
type Driver struct {
	cfg     Config
	network netip.Prefix

	mu        sync.Mutex
	instances map[string]*vm.Instance
	// slots tracks which guest index each instance holds. An index fixes the
	// guest's /30, its TAP device name and its forwarded SSH port, so it must
	// be released only when the instance is destroyed.
	slots    map[string]int
	usedSlot map[int]bool
}

// New returns a Firecracker driver for the host it runs on.
func New(cfg Config) (*Driver, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	network, err := netip.ParsePrefix(cfg.Network)
	if err != nil {
		return nil, fmt.Errorf("firecracker: parse network: %w", err)
	}
	if cfg.Total == (vm.Resources{}) {
		probed, err := probeHost()
		if err != nil {
			return nil, fmt.Errorf("firecracker: probe host capacity: %w", err)
		}
		cfg.Total = probed.Sub(cfg.HostReserve)
	}
	return &Driver{
		cfg:       cfg,
		network:   network.Masked(),
		instances: map[string]*vm.Instance{},
		slots:     map[string]int{},
		usedSlot:  map[int]bool{},
	}, nil
}

// Name implements vm.Driver.
func (d *Driver) Name() string { return "firecracker" }

// Capacity implements vm.Driver.
func (d *Driver) Capacity(context.Context) (vm.Capacity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	capacity := vm.Capacity{Total: d.cfg.Total, MaxInstances: d.cfg.MaxInstances}
	for _, inst := range d.instances {
		if inst.State == vm.StateRunning || inst.State == vm.StatePending {
			capacity.Used = capacity.Used.Add(inst.Spec.Resources)
			capacity.Instances++
		}
	}
	return capacity, nil
}

// Create implements vm.Driver.
//
// Booting a guest is not yet implemented. Everything up to the boot -- slot
// allocation, networking, the chroot layout and the machine configuration --
// is done here so that the remaining work is confined to launching the jailer
// and waiting for the guest to come up.
func (d *Driver) Create(_ context.Context, spec vm.Spec) (*vm.Instance, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("firecracker: booting guests is not implemented yet: %w", vm.ErrNotSupported)
}

// Get implements vm.Driver.
func (d *Driver) Get(_ context.Context, instanceID string) (*vm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	inst, ok := d.instances[instanceID]
	if !ok {
		return nil, fmt.Errorf("firecracker: %s: %w", instanceID, vm.ErrNotFound)
	}
	clone := *inst
	return &clone, nil
}

// List implements vm.Driver.
func (d *Driver) List(context.Context) ([]*vm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*vm.Instance, 0, len(d.instances))
	for _, inst := range d.instances {
		clone := *inst
		out = append(out, &clone)
	}
	return out, nil
}

// Destroy implements vm.Driver.
func (d *Driver) Destroy(_ context.Context, instanceID string) error {
	return fmt.Errorf("firecracker: tearing down guests is not implemented yet: %w", vm.ErrNotSupported)
}

// Exec implements vm.Driver.
func (d *Driver) Exec(_ context.Context, instanceID string, _ vm.Command) (*vm.ExecResult, error) {
	return nil, fmt.Errorf("firecracker: in-guest exec is not implemented yet: %w", vm.ErrNotSupported)
}

// ReserveSlot claims the lowest free guest index for an instance and returns
// the host-side layout it implies. Slots are reused only after an explicit
// release: dabberz never reclaims a VM on its own, so a slot stays held for as
// long as its guest exists.
func (d *Driver) ReserveSlot(instanceID string) (Guest, error) {
	if instanceID == "" {
		return Guest{}, errors.New("firecracker: reserving a slot requires an instance id")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if slot, held := d.slots[instanceID]; held {
		return d.Layout(slot)
	}
	for slot := 0; ; slot++ {
		if d.usedSlot[slot] {
			continue
		}
		// Layout fails once the slot walks off the end of the network, which
		// bounds this loop.
		guest, err := d.Layout(slot)
		if err != nil {
			return Guest{}, err
		}
		d.usedSlot[slot] = true
		d.slots[instanceID] = slot
		return guest, nil
	}
}

// ReleaseSlot frees an instance's guest index for reuse.
func (d *Driver) ReleaseSlot(instanceID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if slot, held := d.slots[instanceID]; held {
		delete(d.usedSlot, slot)
		delete(d.slots, instanceID)
	}
}

// Guest describes the host-side resources one microVM occupies. Every field is
// derived from the guest's slot index, so the whole layout is reproducible
// after a control-plane restart without persisting it.
type Guest struct {
	// Slot is the guest index within the driver.
	Slot int `json:"slot"`
	// TapDevice is the host TAP interface enslaved to the bridge.
	TapDevice string `json:"tap_device"`
	// HostIP and GuestIP are the two usable addresses of the guest's /30.
	HostIP  netip.Addr `json:"host_ip"`
	GuestIP netip.Addr `json:"guest_ip"`
	// PrefixLen is the guest subnet's mask length.
	PrefixLen int `json:"prefix_len"`
	// SSHPort is the host port forwarded to the guest's SSH daemon.
	SSHPort int `json:"ssh_port"`
	// ChrootDir is the jailer chroot for this guest.
	ChrootDir string `json:"chroot_dir"`
	// MAC is derived from the guest IP so it is stable across reboots.
	MAC string `json:"mac"`
}

// guestSubnetSize is the number of addresses per guest: network, host, guest,
// broadcast.
const guestSubnetSize = 4

// Layout computes the host-side resources for a guest slot.
func (d *Driver) Layout(slot int) (Guest, error) {
	if slot < 0 {
		return Guest{}, fmt.Errorf("firecracker: slot %d is negative", slot)
	}
	base := d.network.Addr()
	if !base.Is4() {
		return Guest{}, fmt.Errorf("firecracker: network %s is not IPv4", d.network)
	}

	offset := slot * guestSubnetSize
	hostIP, ok := addOffset(base, offset+1)
	if !ok {
		return Guest{}, fmt.Errorf("firecracker: slot %d overflows network %s", slot, d.network)
	}
	guestIP, ok := addOffset(base, offset+2)
	if !ok {
		return Guest{}, fmt.Errorf("firecracker: slot %d overflows network %s", slot, d.network)
	}
	// The whole /30 must stay inside the configured network, or two guests
	// would end up sharing addresses with something outside the pool.
	if last, ok := addOffset(base, offset+guestSubnetSize-1); !ok || !d.network.Contains(last) {
		return Guest{}, fmt.Errorf("firecracker: slot %d overflows network %s", slot, d.network)
	}

	return Guest{
		Slot:      slot,
		TapDevice: fmt.Sprintf("fc-tap%d", slot),
		HostIP:    hostIP,
		GuestIP:   guestIP,
		PrefixLen: 30,
		SSHPort:   d.cfg.SSHPortBase + slot,
		ChrootDir: filepath.Join(d.cfg.ChrootBase, fmt.Sprintf("slot-%d", slot)),
		MAC:       macForIP(guestIP),
	}, nil
}

// addOffset advances an IPv4 address by n, reporting false on overflow.
func addOffset(addr netip.Addr, n int) (netip.Addr, bool) {
	if !addr.Is4() || n < 0 {
		return netip.Addr{}, false
	}
	raw := addr.As4()
	value := uint64(raw[0])<<24 | uint64(raw[1])<<16 | uint64(raw[2])<<8 | uint64(raw[3])
	sum := value + uint64(n)
	if sum > 0xFFFFFFFF {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{
		byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum),
	}), true
}

// macForIP derives a stable locally-administered MAC from a guest address, so
// a rebooted guest keeps its identity on the bridge.
func macForIP(addr netip.Addr) string {
	raw := addr.As4()
	// 0x02 marks the address as locally administered and unicast.
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", raw[0], raw[1], raw[2], raw[3])
}

// MachineConfig is the Firecracker machine configuration for one guest. The
// field names match the Firecracker API so the struct can be PUT to the
// guest's API socket unchanged.
type MachineConfig struct {
	BootSource struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args"`
	} `json:"boot-source"`
	Drives        []Drive `json:"drives"`
	MachineConfig struct {
		VCPUCount  int  `json:"vcpu_count"`
		MemSizeMiB int  `json:"mem_size_mib"`
		Smt        bool `json:"smt"`
	} `json:"machine-config"`
	NetworkInterfaces []NetworkInterface `json:"network-interfaces"`
}

// Drive is one block device attached to a guest.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface attaches a guest to its TAP device.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// BuildMachineConfig renders the Firecracker configuration for a guest.
//
// rootfsPath is the guest's own copy-on-write overlay of the golden image, not
// the golden image itself: guests get a messy, disposable environment and must
// never write back to the shared base.
func (d *Driver) BuildMachineConfig(spec vm.Spec, guest Guest, rootfsPath string) (*MachineConfig, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if rootfsPath == "" {
		return nil, errors.New("firecracker: a per-guest rootfs overlay path is required")
	}

	cfg := &MachineConfig{}
	cfg.BootSource.KernelImagePath = d.cfg.KernelImage
	// Static addressing via the kernel command line avoids needing DHCP inside
	// the golden image.
	cfg.BootSource.BootArgs = fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:%s::eth0:off",
		guest.GuestIP, guest.HostIP, prefixMask(guest.PrefixLen),
	)
	cfg.Drives = []Drive{{
		DriveID:      "rootfs",
		PathOnHost:   rootfsPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}}
	cfg.MachineConfig.VCPUCount = spec.Resources.VCPUs
	cfg.MachineConfig.MemSizeMiB = spec.Resources.MemoryMiB
	cfg.NetworkInterfaces = []NetworkInterface{{
		IfaceID:     "eth0",
		GuestMAC:    guest.MAC,
		HostDevName: guest.TapDevice,
	}}
	return cfg, nil
}

// prefixMask renders a prefix length as a dotted-quad netmask for the kernel
// command line, which does not accept CIDR notation.
func prefixMask(bits int) string {
	if bits < 0 || bits > 32 {
		return "255.255.255.255"
	}
	mask := ^uint32(0) << (32 - bits)
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

// WriteMachineConfig renders a guest's configuration to disk in the form the
// Firecracker binary accepts via --config-file.
func (d *Driver) WriteMachineConfig(path string, cfg *MachineConfig) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("firecracker: encode machine config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("firecracker: create config directory: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("firecracker: write machine config: %w", err)
	}
	return nil
}

// JailerArgs renders the jailer invocation for a guest. The jailer is what
// puts each guest in its own chroot, uid and cgroup, so guests are isolated
// from the control plane as well as from each other.
func (d *Driver) JailerArgs(instanceID string, guest Guest, configPath string) []string {
	return []string{
		d.cfg.JailerBin,
		"--id", instanceID,
		"--exec-file", d.cfg.FirecrackerBin,
		// Run each guest under its own uid/gid derived from its slot, so a
		// guest escaping Firecracker still cannot touch its siblings' files.
		"--uid", fmt.Sprint(jailerBaseUID + guest.Slot),
		"--gid", fmt.Sprint(jailerBaseUID + guest.Slot),
		"--chroot-base-dir", d.cfg.ChrootBase,
		"--",
		"--config-file", configPath,
	}
}

// jailerBaseUID is the first uid handed to a jailed guest. It sits well above
// the system range so guest uids never collide with real accounts.
const jailerBaseUID = 30000

// Driver satisfies the vm.Driver contract.
var _ vm.Driver = (*Driver)(nil)
