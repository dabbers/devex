#!/usr/bin/env bash
# Build the golden image Firecracker guests boot: an ext4 filesystem on a ZFS
# zvol, with the dabberz guest agent installed, snapshotted so that each guest
# boots a copy-on-write clone of the snapshot.
#
#   sudo deploy/firecracker/build-golden.sh <rootdir> <agent-binary> <pool/zvol> [size]
#
# <rootdir> is a Debian tree; it is made with debootstrap if it does not exist.
# <agent-binary> is a static build of cmd/dabberz-guest
#   (CGO_ENABLED=0 go build -o dabberz-guest ./cmd/dabberz-guest).
# The result is <pool/zvol>@base, the value for driver.firecracker.golden_rootfs.
#
# This is the smallest image that proves the driver, not the toolchain image
# the product needs; that one is built the same way with more packages.
set -euo pipefail

root=${1:?rootdir}
agent=${2:?agent binary}
zvol=${3:?pool/zvol}
size=${4:-4G}
suite=${SUITE:-trixie}
mirror=${MIRROR:-https://deb.debian.org/debian}

if [[ $(id -u) -ne 0 ]]; then
  echo "build-golden: must run as root" >&2
  exit 1
fi
if zfs list -H "$zvol" >/dev/null 2>&1; then
  echo "build-golden: $zvol already exists; destroy it (and its clones) first" >&2
  exit 1
fi

if [[ ! -e $root/etc/os-release ]]; then
  debootstrap --variant=minbase \
    --include=systemd-sysv,udev,iproute2,iputils-ping,e2fsprogs,ca-certificates,git,procps,less \
    "$suite" "$root" "$mirror"
fi

install -m 0755 "$agent" "$root/usr/local/sbin/dabberz-guest"
# The agent starts as early as systemd can start anything: the driver waits
# for it, so every unit ordered before it is boot time an agent waits through.
cat >"$root/etc/systemd/system/dabberz-guest.service" <<'UNIT'
[Unit]
Description=dabberz guest agent
DefaultDependencies=no
After=local-fs.target
[Service]
ExecStart=/usr/local/sbin/dabberz-guest
# The console is the only record of a guest that never answered, and the
# driver hands it back to whoever's Create failed.
StandardOutput=journal+console
StandardError=journal+console
Restart=always
RestartSec=200ms
[Install]
WantedBy=sysinit.target
UNIT
mkdir -p "$root/etc/systemd/system/sysinit.target.wants"
ln -sf /etc/systemd/system/dabberz-guest.service \
  "$root/etc/systemd/system/sysinit.target.wants/dabberz-guest.service"

# Nobody logs in on a guest's virtual terminals; not starting six gettys is
# boot time back.
ln -sf /dev/null "$root/etc/systemd/system/getty.target"

mkdir -p "$root/workspace"
echo dabberz-guest >"$root/etc/hostname"
# The kernel configures eth0 from the boot command line; nothing in the guest
# should try to manage it again.
cat >"$root/etc/fstab" <<'FSTAB'
/dev/vda / ext4 defaults,noatime 0 1
FSTAB
# No password login anywhere: the way in is the agent, from the host.
chroot "$root" passwd -l root >/dev/null

# Sparse: the pool is shared with production, and clones need no reservation.
zfs create -p -s -V "$size" -o volmode=dev "$zvol"
device=/dev/zvol/$zvol
for _ in $(seq 100); do [[ -e $device ]] && break; sleep 0.1; done
[[ -e $device ]] || { echo "build-golden: $device did not appear" >&2; exit 1; }
mkfs.ext4 -q -F -L rootfs -d "$root" "$device"
zfs snapshot "$zvol@base"
echo "golden_rootfs: $zvol@base"
