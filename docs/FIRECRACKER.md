# The Firecracker driver: what works and what does not

Status as of 2026-11-18, proved on dbz-alpha (Debian 13, Xeon E5-1650 v4, ZFS
pool `tank` on spinning disks), Firecracker v1.18.0, guest kernel 6.1.155 from
Firecracker's CI artifacts, golden image: minimal Debian trixie with systemd.

**It is not wired into the running deployment.** dabberzd on dbz-alpha still
runs the `local` driver. Nothing below has touched `/etc/dabberz`, the systemd
units, the nftables table or the production Postgres.

## How a guest is built

| Piece | How | Why |
|---|---|---|
| Disk | `zfs clone <golden>@base <clone_dataset>/<id>`; grown with `volsize` and an online `resize2fs` when `disk_gib` exceeds the image | The copy-on-write rule: a clone measured 373 ms, growing it to 40 GiB 160 ms. Nothing is copied. |
| Jail | jailer, uid/gid `30000+slot`, chroot `<chroot_base>/firecracker/<id>/root`, own mount namespace | The jail holds one block device, a node for the guest's own clone. The kernel is hard-linked in, root-owned. |
| Network | TAP `fc-tapN` with the host side of the guest's /30 assigned to it; the guest is configured by `ip=` on the kernel command line | One /30 per guest is already a point-to-point link, so there is no bridge. `bridge_name` is now unused. |
| Exec | `cmd/dabberz-guest`, a static Go agent started by systemd early in boot, on vsock port 1024; host side is Firecracker's hybrid-vsock Unix socket in the jail | No keys to provision. Works with the guest's network broken. Not reachable from the guest network. Protocol in `internal/vm/firecracker/guestproto`. |
| Sessions | The same agent allocates a pty in the guest; `vm.Interactive` and `vm.Streaming` are both implemented | |

The golden image is built by `deploy/firecracker/build-golden.sh`, which takes
a debootstrap tree, the agent binary and a zvol name and leaves `<zvol>@base`.
`golden_rootfs` is now a ZFS snapshot name, and `Config.Validate` rejects a file
path, which is the mistake the copy-on-write rule exists to prevent.

## Measured

Time to first exec for a fresh guest (1 vCPU, 512 MiB): **2.0 to 2.5 s**, from
`Create` called to the first `Exec` returning. Across six runs: 2.04, 2.20,
2.26, 2.29, 2.50 and 2.20 s. Almost all of it is the guest's systemd boot.
`Create` returns once the agent answers, and the first `Exec` then takes 2 to
6 ms.

## Tests

`internal/vm/firecracker/boot_linux_test.go` boots real guests. Its tests skip
without root, `/dev/kvm` and the `DABBERZ_FC_TEST_*` variables listed at the
top of the file. On dbz-alpha:

```
B=/tank/dabberz/fc-dev   # torn down after this session; recreate as below
sudo env PATH=$PATH HOME=/root \
  DABBERZ_FC_TEST_BIN=$B/bin DABBERZ_FC_TEST_KERNEL=$B/jail/vmlinux \
  DABBERZ_FC_TEST_GOLDEN=tank/dabberz/fc-dev/golden@base DABBERZ_FC_TEST_JAIL=$B/jail \
  DABBERZ_FC_TEST_CLONES=tank/dabberz/fc-dev/instances \
  go test -v ./internal/vm/firecracker
```

They cover: running in the guest's kernel, not the host's; stdin, stdout,
stderr and exit codes; a missing binary reported as an error; the timeout
killing the whole process group; streaming; eth0 matching the layout and the
host reachable; writes staying in a guest's own clone, whose origin is the
golden snapshot; disk growth; the jail's uid, root and single disk; Destroy
removing the process, TAP, clone, jail and capacity, then ErrNotFound; a failed
boot leaving nothing behind; a pty session.

Each guard was removed and its test was seen to fail: the resize step, the
clone (booting the golden zvol directly), cleanup after a failed boot, TAP
removal, clone removal, capacity release, the per-slot uid, and the agent
killing the process group rather than only the leader.

To recreate the fixtures (downloads approved for this session: Firecracker
v1.18.0 from its GitHub release, checked against the release's sha256; the
6.1 kernel from `s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/`;
debootstrap from deb.debian.org):

```
sudo zfs create -p tank/dabberz/fc-dev/jail     # kernel and jails share it, so the kernel is linked
# firecracker, jailer into $B/bin; vmlinux into $B/jail (chmod 0644)
CGO_ENABLED=0 go build -o $B/bin/dabberz-guest ./cmd/dabberz-guest
sudo deploy/firecracker/build-golden.sh $B/build/root $B/bin/dabberz-guest tank/dabberz/fc-dev/golden 4G
```

## Not done, in the order it matters

1. **Guests can reach the host and each other.** Measured from a guest: it
   pings another guest's address, since the host has `net.ipv4.ip_forward=1`,
   and it opens TCP to the host's sshd (:22) and :41000 on its TAP gateway
   address. It has no internet route, because nothing NATs 172.30/16. Before a
   second customer, the driver needs a filter of its own, a separate nftables
   table and not `dabberz_edge`. It should drop forwarding between `fc-tap*`
   interfaces and input from them to the host, except whatever the guest is
   meant to reach. Egress, meaning NAT plus a policy, is a decision still to
   make.
2. **Not wired into dabberzd.** Switching `driver.kind` to `firecracker` also
   needs the deployment to ship the binaries, kernel, golden image and agent
   (bootstrap-host.sh does none of this), and dabberzd to run as root or with
   the capabilities jailer, zfs and ip need.
3. **No reconciliation after a restart.** Instance state lives in memory. If
   dabberzd restarts, the driver forgets its guests. Whether they keep running
   depends on the unit's KillMode, since they are in their own session.
   Either way their clones, TAPs and jails leak, and their slots are reused. The pieces are named after the instance id and slot, so
   rebuilding the state from `zfs list -r <clone_dataset>` and the jail
   directories is straightforward; it just is not written.
4. **No cgroup limits from the jailer.** vCPU and memory are bounded by the VM
   itself, but disk and network I/O are not, and neither is the Firecracker
   process's own overhead.
5. **The golden image is minimal.** It has no toolchain, no browser and no
   sshd. `SSHPort` is reserved per slot but nothing forwards it.
   `vm.Instance.SSHPort` is left at zero.
6. **The 6.1 CI kernel is Firecracker's test kernel**, not one this project
   builds or tracks for security updates.
7. `Spec.Image` is ignored: there is one golden image, as v1 intends.
