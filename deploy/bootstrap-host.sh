#!/usr/bin/env bash
#
# Take a fresh Debian 13 host to the point where a Claude Code session on the
# machine can take over.
#
# Deliberately stops short of configuring dabberz itself. Installing packages,
# building a storage pool and putting a toolchain on a box are the parts worth
# having written down and repeatable; writing a config with a domain and a
# master key in it is the part worth doing with a person present.
#
# Everything here is idempotent: run it again after fixing something and it
# picks up where it stopped rather than doubling up.
#
# The one destructive step -- partitioning the free space and building the
# pool -- does nothing unless asked for with --storage. Without it the script
# prints the exact commands it would have run, so they can be read before they
# are trusted.
#
#   ./bootstrap-host.sh              # everything except touching disks
#   ./bootstrap-host.sh --storage    # also partition free space and make the pool
#   ./bootstrap-host.sh --help
#
# Getting it onto a fresh host, which has no git yet -- that is what this
# installs. Fetch the one file, read it, then run it:
#
#   apt-get update && apt-get install -y curl
#   curl -fsSL -o bootstrap-host.sh \
#     https://raw.githubusercontent.com/dabbers/devex/refs/heads/claude/trusting-brown-dns9i5/deploy/bootstrap-host.sh
#   less bootstrap-host.sh
#   bash bootstrap-host.sh
#
set -euo pipefail

readonly POOL=tank
readonly SERVICE_USER=dabberz
readonly REPO_URL=https://github.com/dabbers/devex.git
# The branch this work lives on. Not the default branch, which is a long way
# behind -- cloning that would hand the session a tree without any of it.
readonly REPO_BRANCH=claude/trusting-brown-dns9i5
readonly CHECKOUT=/opt/dabberz/src
# The floor from go.mod, not a pin. Pinning a series here means this file goes
# stale and the script fails on a box that is perfectly capable of building.
readonly GO_MINIMUM=1.25.0
# Printed by `gpg --show-keys` for Anthropic's signing key. Checked rather than
# trusted, so a substituted key fails here instead of installing quietly.
# What `sgdisk --typecode=0:bf01` writes: "Solaris /usr & Mac ZFS", which is
# what ZFS on Linux uses. Read off a real partition rather than copied from
# memory. Compared lowercase, which is how lsblk reports it.
readonly ZFS_PARTTYPE=6a898cc3-1dd2-11b2-99a6-080020736631
# Printed by `gpg --show-keys` for Anthropic's signing key. Checked rather than
# trusted, so a substituted key fails here instead of installing quietly.
readonly CLAUDE_KEY_FPR=31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE

DO_STORAGE=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
warn() { printf '\033[33m   ! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31m   x %s\033[0m\n' "$*" >&2; exit 1; }

# cluster_ready reports whether the local cluster answers. Timed out rather
# than waited on: the failure it replaces was psql reading a heredoc from a
# socket nobody was listening on, which produces no output and never returns.
cluster_ready() {
	timeout 15 runuser -u postgres -- psql -tAc 'select 1' >/dev/null 2>&1
}

# at_least reports whether $1 is version $2 or newer.
at_least() {
	[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$2" ]
}

usage() {
	# Derived from the header rather than a line range, which goes stale the
	# first time somebody adds a paragraph to it.
	awk 'NR<3 {next} /^#/ {sub(/^# ?/, ""); print; next} {exit}' "$0"
	exit 0
}

for arg in "$@"; do
	case "$arg" in
		--storage) DO_STORAGE=1 ;;
		-h|--help) usage ;;
		*) die "unknown argument: $arg" ;;
	esac
done

# ---------------------------------------------------------------------------

preflight() {
	say "Preflight"

	[ "$(id -u)" -eq 0 ] || die "run as root"

	local version
	version=$(. /etc/os-release && echo "${ID:-}-${VERSION_ID:-}")
	case "$version" in
		debian-13) note "Debian 13, as expected" ;;
		debian-*)  warn "this is $version; written for debian-13, continuing" ;;
		*)         die "this is $version; written for Debian 13" ;;
	esac

	# The production driver needs KVM, and a host without it will install
	# perfectly and then fail at the only thing that matters. On bare metal
	# this is present; on a VM it usually is not.
	if [ -e /dev/kvm ]; then
		note "/dev/kvm present"
	else
		warn "/dev/kvm is missing -- Firecracker cannot run here, only the local driver"
	fi

	note "$(nproc) cpus, $(free -g | awk '/^Mem:/ {print $2}') GiB ram"
	note "root filesystem: $(df -h / | awk 'NR==2 {print $4" free of "$2}')"
}

packages() {
	say "Packages"

	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq

	# Grouped by what needs them, so a future reader can tell which lines are
	# safe to drop.
	apt-get install -y -qq \
		ca-certificates curl gnupg \
		git jq \
		debootstrap e2fsprogs gdisk parted util-linux \
		iproute2 nftables \
		chromium \
		postgresql postgresql-client \
		zfsutils-linux \
		>/dev/null

	note "installed: git, chromium, postgres, debootstrap, gdisk, nftables"

	# zfsutils-linux is the userspace half. Without the module it installs
	# perfectly and then every zpool command fails, which is how the disks got
	# partitioned for a pool that could not be created.
	if ! modinfo zfs >/dev/null 2>&1; then
		note "building the zfs kernel module (dkms, a few minutes)"
		apt-get install -y -qq "linux-headers-$(uname -r)" >/dev/null 2>&1 ||
			apt-get install -y -qq linux-headers-amd64 >/dev/null
		apt-get install -y zfs-dkms >/dev/null
	fi
	if modprobe zfs 2>/dev/null && modinfo zfs >/dev/null 2>&1; then
		note "zfs module $(modinfo -F version zfs 2>/dev/null || echo loaded)"
	else
		warn "the zfs module is still unavailable; the pool cannot be built"
	fi

	# Guests reach the internet through the host once the Firecracker driver
	# lands. Harmless until then, and awkward to debug when forgotten.
	if ! grep -qs '^net.ipv4.ip_forward=1' /etc/sysctl.d/99-dabberz.conf; then
		echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-dabberz.conf
		sysctl -q --system
		note "enabled ip_forward for guest networking"
	fi
}

# storage finds the unallocated space left at install time and makes a mirrored
# pool out of it.
#
# Everything dabberz holds goes here rather than on the 97.7 GiB root: Postgres,
# the bare repositories, the workspace tree, the guest rootfs overlays, and the
# snapshot directory that nothing prunes yet. Root is for the OS.
storage() {
	say "Storage"

	if zpool list -H -o name 2>/dev/null | grep -qx "$POOL"; then
		note "pool '$POOL' already exists"
		zfs list -H -o name,used,avail -r "$POOL" | sed 's/^/   /'
		return
	fi

	# Before any disk is touched, not after. Partitioning for a pool that
	# cannot then be created leaves the disks changed and the job undone, and
	# the next run finds no free space because the partitions are already
	# sitting in it.
	if ! modprobe zfs 2>/dev/null || ! zpool list >/dev/null 2>&1; then
		warn "zfs is not usable on this kernel, so no disk will be touched"
		note "the module is built by dkms against $(uname -r):"
		note "  apt-get install -y linux-headers-$(uname -r) zfs-dkms"
		note "  modprobe zfs"
		return
	fi

	# Partitions already typed for ZFS and not yet claimed by anything. This
	# is what a resumed run finds: the partitions exist from last time, so
	# there is no free space left to look for.
	# Parsed rather than eval'd. lsblk -P emits shell-looking key="value"
	# pairs and it is tempting to just eval the line, which is a device
	# listing being executed as code the first time a name contains a quote.
	local -a parts=()
	local line name parttype fstype
	while IFS= read -r line; do
		name=$(sed -n 's/.*\bNAME="\([^"]*\)".*/\1/p' <<<"$line")
		parttype=$(sed -n 's/.*\bPARTTYPE="\([^"]*\)".*/\1/p' <<<"$line")
		fstype=$(sed -n 's/.*\bFSTYPE="\([^"]*\)".*/\1/p' <<<"$line")

		[ -n "$name" ] || continue
		# Claimed by a pool or anything else: not ours to take.
		[ -z "$fstype" ] || continue
		[ "${parttype,,}" = "$ZFS_PARTTYPE" ] || continue
		parts+=("/dev/$name")
	done < <(lsblk -P -o NAME,PARTTYPE,FSTYPE)

	if [ "${#parts[@]}" -gt 0 ]; then
		note "found unused zfs partitions from an earlier run: ${parts[*]}"
	else
		# Only disks that already carry a partition table with partitions on
		# them, which on this machine means the two the installer laid out. A
		# blank or foreign disk is deliberately left alone: initialising
		# something unrecognised is how a script eats the wrong device.
		local -a targets=()
		local disk free
		while read -r disk; do
			case "$disk" in zram*|loop*|ram*|sr*) continue ;; esac
			sfdisk -l "/dev/$disk" 2>/dev/null | grep -q '^Disklabel type' || continue
			[ "$(lsblk -ln -o NAME "/dev/$disk" | tail -n +2 | wc -l)" -gt 0 ] || continue

			# Rows matched by shape rather than indentation. sfdisk indents the
			# header and starts the data rows at column zero, which is the
			# opposite way round from what it looks like, and a leading-space
			# pattern silently sums nothing and reports a full disk as empty.
			free=$(sfdisk --list-free "/dev/$disk" 2>/dev/null |
				awk '$1 ~ /^[0-9]+$/ && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ {s+=$3} END{print s+0}')
			[ "${free:-0}" -gt 0 ] || continue

			targets+=("/dev/$disk")
			note "$(printf '/dev/%s has %s unpartitioned' "$disk" \
				"$(numfmt --to=iec --suffix=B $((free * 512)))")"
		done < <(lsblk -dn -o NAME,TYPE | awk '$2=="disk" {print $1}')

		if [ "${#targets[@]}" -eq 0 ]; then
			warn "no partitioned disk has free space; nothing to build a pool from"
			return
		fi

		if [ "$DO_STORAGE" -ne 1 ]; then
			warn "not touching disks -- re-run with --storage to do it"
			note "it would add one partition to each of: ${targets[*]}"
			note "then: zpool create -o ashift=12 -O compression=lz4 -O atime=off $POOL [mirror] <new partitions>"
			return
		fi

		# The new partitions are discovered rather than predicted. Working out
		# the name in advance means assuming sgdisk picks the next number and
		# that the existing ones have no gaps -- this machine produced sda5 and
		# sdb6 -- and being wrong points zpool at a device somebody is using.
		local before after
		before=$(lsblk -ln -o NAME | sort)
		for disk in "${targets[@]}"; do
			sgdisk --new=0:0:0 --typecode=0:bf01 "$disk" >/dev/null
		done
		partprobe "${targets[@]}" 2>/dev/null || true
		udevadm settle || true
		after=$(lsblk -ln -o NAME | sort)

		while read -r name; do
			[ -n "$name" ] && parts+=("/dev/$name")
		done < <(comm -13 <(echo "$before") <(echo "$after"))

		[ "${#parts[@]}" -eq "${#targets[@]}" ] ||
			die "expected ${#targets[@]} new partitions, found ${#parts[@]}: ${parts[*]:-none}"
		note "created: ${parts[*]}"
	fi

	if [ "$DO_STORAGE" -ne 1 ]; then
		warn "not creating the pool -- re-run with --storage to do it"
		note "it would run: zpool create ... $POOL [mirror] ${parts[*]}"
		return
	fi

	local vdev="${parts[*]}"
	[ "${#parts[@]}" -ge 2 ] && vdev="mirror ${parts[*]}"

	# ashift=12 because these are 4K-sector disks and it cannot be changed
	# afterwards without destroying the pool. compression is close to free and
	# these disks are spinning, where saving a read is worth the CPU.
	# shellcheck disable=SC2086
	zpool create -o ashift=12 -O compression=lz4 -O atime=off "$POOL" $vdev
	note "created pool '$POOL' over ${#parts[@]} device(s)"

	# recordsize=8k on the Postgres dataset to match its page size. On a
	# copy-on-write pool the default 128k turns an 8k write into a 128k
	# read-modify-write, which on spinning disks is the difference that gets
	# noticed first.
	zfs create -o recordsize=8k "$POOL/postgres"
	zfs create "$POOL/dabberz"
	zfs create "$POOL/dabberz/data"      # control plane sqlite, bare repos
	zfs create "$POOL/dabberz/vm"        # workspaces and guest rootfs overlays
	zfs create "$POOL/dabberz/snapshots" # one pg_dump per promotion
	note "datasets: postgres (8k records), dabberz/{data,vm,snapshots}"
}

golang() {
	say "Go toolchain"

	# Needed on the host to build dabberzd, and inside the golden image too,
	# because an app's own build step runs there.
	local have
	if command -v go >/dev/null; then
		have=$(go version | awk '{print $3}' | sed 's/^go//')
		if at_least "$have" "$GO_MINIMUM"; then
			note "already present: go$have (>= $GO_MINIMUM)"
			return
		fi
		note "go$have is older than $GO_MINIMUM; replacing it"
	fi

	# Whatever is current and stable, checksum-verified. The floor is checked
	# after resolving, so a toolchain too old to build this is caught here
	# rather than by a confusing compile error later.
	local release url sha archive version
	release=$(curl -fsSL 'https://go.dev/dl/?mode=json' |
		jq -r '[.[] | select(.stable==true)][0]
		       | .version as $v
		       | (.files[] | select(.os=="linux" and .arch=="amd64" and .kind=="archive")
		          | "\($v)\t\(.filename)\t\(.sha256)")')
	[ -n "$release" ] && [ "$release" != "null" ] || die "could not resolve a stable Go release"

	version=$(cut -f1 <<<"$release" | sed 's/^go//')
	archive=$(cut -f2 <<<"$release")
	sha=$(cut -f3 <<<"$release")
	url="https://go.dev/dl/$archive"

	at_least "$version" "$GO_MINIMUM" ||
		die "the current stable Go is $version, which is below the required $GO_MINIMUM"

	note "fetching $archive"
	curl -fsSL "$url" -o "/tmp/$archive"
	echo "$sha  /tmp/$archive" | sha256sum -c - >/dev/null || die "checksum mismatch on $archive"

	rm -rf /usr/local/go
	tar -C /usr/local -xzf "/tmp/$archive"
	rm -f "/tmp/$archive"
	printf 'export PATH=$PATH:/usr/local/go/bin\n' > /etc/profile.d/golang.sh
	note "installed $(/usr/local/go/bin/go version)"
}

claude_code() {
	say "Claude Code"

	if command -v claude >/dev/null; then
		note "already present: $(claude --version 2>/dev/null || echo unknown)"
		return
	fi

	# The apt repository rather than the install script. On a server, packages
	# that are signed, OS-managed and do not update themselves underneath a
	# running session are worth more than the convenience of curl | bash.
	install -d -m 0755 /etc/apt/keyrings
	curl -fsSL https://downloads.claude.ai/keys/claude-code.asc \
		-o /etc/apt/keyrings/claude-code.asc

	# Checked, not assumed. A key fetched over TLS is only as good as whoever
	# answered, and this is the one line that makes that verifiable.
	if ! gpg --show-keys --with-colons /etc/apt/keyrings/claude-code.asc 2>/dev/null |
		awk -F: '$1=="fpr"{print $10}' | grep -qx "$CLAUDE_KEY_FPR"; then
		rm -f /etc/apt/keyrings/claude-code.asc
		die "the signing key did not match the expected fingerprint; refusing to add the repository"
	fi
	note "signing key matches $CLAUDE_KEY_FPR"

	echo "deb [signed-by=/etc/apt/keyrings/claude-code.asc] https://downloads.claude.ai/claude-code/apt/stable stable main" \
		> /etc/apt/sources.list.d/claude-code.list

	DEBIAN_FRONTEND=noninteractive apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq claude-code >/dev/null
	note "installed $(claude --version 2>/dev/null || echo 'claude-code')"
	note "the stable channel does not self-update; apt upgrade claude-code when wanted"
}

postgres() {
	say "Postgres"

	local creds=/etc/dabberz/data.env
	if [ -f "$creds" ]; then
		note "admin credentials already exist at $creds"
		return
	fi

	# Checked before anything is attempted, with a timeout, because the
	# failure this replaces was a silent hang: psql reading a heredoc from a
	# cluster that is not listening produces no output at all, and the script
	# simply stops with the section heading printed and nothing after it.
	if ! cluster_ready; then
		# Debian's postgresql package normally creates and starts a cluster on
		# install. It does not always: a debconf step that cannot find
		# pg_lsclusters leaves the package installed and no cluster behind,
		# and the first sign of it is this script stopping with no output.
		# Recovered rather than reported, because making the cluster is what
		# the package was supposed to have done anyway.
		local major
		major=$(psql --version | awk '{print $3}' | cut -d. -f1)
		warn "no cluster is accepting connections; creating ${major}/main"
		pg_createcluster "$major" main --start >/dev/null 2>&1 || true

		if ! cluster_ready; then
			warn "postgres is still not accepting connections on the local socket"
			note "check:  pg_lsclusters"
			note "        systemctl status postgresql@${major}-main"
			note "        journalctl -u postgresql@${major}-main --no-pager -n 30"
			die "cannot create the dabberz role without a running cluster"
		fi
		note "cluster ${major}/main is up"
	fi

	# The data block connects as a role that can create databases and roles,
	# because provisioning an installation is exactly that. Not the bootstrap
	# superuser: the restore path passes --role=, and a role that owns nothing
	# is easier to reason about than one that owns the cluster.
	# From urandom rather than openssl, which a minimal Debian may not carry.
	# Alphanumerics only, so it needs no escaping in either a SQL literal or a
	# connection string.
	#
	# Written in two steps on purpose. The obvious one line --
	# `tr -dc ... </dev/urandom | head -c 32` -- ends the pipeline with a
	# consumer that exits early, so tr takes SIGPIPE, pipefail turns that into
	# a failed assignment, and errexit kills the script. Silently: the exit
	# status is 141 and nothing is printed, which reads exactly like a hang at
	# whatever ran last. Reading a fixed amount and trimming in the shell
	# leaves nothing to signal.
	local raw password
	raw=$(head -c 512 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')
	password=${raw:0:32}
	[ "${#password}" -eq 32 ] || die "could not generate a password"

	runuser -u postgres -- psql -v ON_ERROR_STOP=1 -q -f - <<-SQL
		DO \$\$ BEGIN
		  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'dabberz') THEN
		    CREATE ROLE dabberz LOGIN CREATEDB CREATEROLE PASSWORD '${password}';
		  END IF;
		END \$\$;
	SQL

	install -d -m 0750 /etc/dabberz
	umask 077
	cat > "$creds" <<-ENV
		# Read by dabberzd. The password is written once, here, and nowhere else.
		DABBERZ_DATA_ADMIN_DSN=postgres://dabberz:${password}@127.0.0.1:5432/postgres?sslmode=disable
	ENV
	chmod 0600 "$creds"
	note "created role 'dabberz' and wrote $creds (0600)"
	note "pg_dump/pg_restore: $(pg_dump --version | awk '{print $3}')"
}

checkout() {
	say "Source"

	if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
		useradd --system --create-home --home-dir /var/lib/dabberz \
			--shell /usr/sbin/nologin "$SERVICE_USER"
		note "created service user '$SERVICE_USER'"
	fi

	install -d -m 0755 "$(dirname "$CHECKOUT")"
	if [ -d "$CHECKOUT/.git" ]; then
		# Reported rather than corrected. If somebody has moved this checkout
		# onto another branch they had a reason, and a bootstrap script is the
		# wrong thing to be resetting their working tree.
		note "already cloned at $CHECKOUT (on $(git -C "$CHECKOUT" branch --show-current))"
	else
		git clone -q --branch "$REPO_BRANCH" "$REPO_URL" "$CHECKOUT"
		note "cloned $REPO_BRANCH to $CHECKOUT"
	fi
}

summary() {
	say "Done"
	cat <<-NEXT
	   The host has what it needs. What is deliberately not done, because it
	   wants a person and a domain name:

	     - the dabberz config (domains, master key, driver choice)
	     - TLS and DNS: one name for the control plane, a wildcard for previews
	     - the systemd unit
	     - moving Postgres onto $POOL/postgres, if the pool was built

	   From here, on this machine:

	     claude
	     > read docs/HOST.md and finish setting this host up

	   Credentials are in /etc/dabberz/data.env, root-only. Source is at
	   $CHECKOUT.
	NEXT
}

preflight
packages
storage
golang
claude_code
postgres
checkout
summary
