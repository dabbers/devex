# The host

What a machine has to provide to run dabberz, why, and which of those choices
cannot be changed afterwards without reinstalling.

This exists because the OS question has one answer that matters and several
that do not. The distribution barely matters. The disk layout, the kernel's
virtualisation support and the DNS split matter a great deal, and two of the
three are decided at install time.

## The honest state of isolation

Read this before deciding who gets an account.

The production driver does not boot guests yet. `internal/vm/firecracker`
carries the address layout, the slot accounting and the machine config, and its
`Create`, `Exec`, `Destroy` and `OpenSession` return `vm.ErrNotSupported`. The
only driver that runs anything today is `local`, which runs agents and services
on the host, as the dabberzd process, with that process's access -- and the
agent acts without asking, because there is nobody to ask.

That combination is right for one person validating their own apps on their own
machine. It is not multi-tenancy. Until Firecracker boots guests, the control
that keeps one customer out of another customer's data is *not* having given
anybody else an account. Cards and quotas limit who signs up and how much they
consume; neither is a substitute for a boundary.

So: provision for Firecracker now, run on `local` while validating, and treat
the first external account as blocked on the driver rather than on billing.

## Debian

Debian 13 "trixie", kernel 6.12 series.

Chosen for one real reason beyond taste: the golden guest image is a Debian
rootfs, and building it on Debian is `debootstrap` and nothing else. Every other
base makes the guest image a cross-distribution exercise for no benefit. The
rest follows -- 6.12 is far past Firecracker's floor, cgroups v2 is the default,
and the archive carries Postgres, Chromium, bridge tooling and `debootstrap`
without third-party repositories.

Ubuntu LTS would also work and is the same family. A RHEL rebuild would work and
would make the golden image harder for no gain. The distribution is the least
consequential decision on this page.

## Decided at install time

These need getting right during OVH's install, because changing them later means
reinstalling.

### Leave the disk unallocated

Do not let the installer consume the whole disk. Partition what is needed now
and leave the rest as free space in the volume group.

The reason is database forking. Opening a change gives it a copy of the
installation's data, and today that copy is `pg_dump` into a fresh database.
That is correct and it is linear in the size of the data: fine for a form with a
few thousand submissions, not fine for the Notion-shaped apps in the catalog
plan. The fix when it arrives is copy-on-write -- a ZFS or btrfs dataset cloned
per change, which turns a minutes-long restore into a constant-time operation --
and that needs a pool on its own storage. Carving one out of free space is an
afternoon. Repartitioning a server with customers on it is not.

So: ext4 on LVM for the system and the data directory, and leave room. If the
machine has two drives, leaving the second one unpartitioned entirely is the
cleanest version of this.

### Somewhere for each thing

The paths are configuration, so the names below are only what the example
config uses. What matters is that they are sized differently and that one of
them is not on the same filesystem as a runaway build.

| Holds | Grows with | Note |
|---|---|---|
| `data_dir` -- control plane SQLite, bare repositories | installations | small, back this up |
| per-instance workspaces | open changes x checkout size | each one is a full checkout plus a build cache |
| Postgres data | customer data | the thing to be careful with |
| snapshot directory | promotions | `pg_dump` output, one per promote, pruned by nobody yet |

The snapshot directory is the one that will surprise you. Promoting snapshots
the live database first, and nothing deletes old snapshots.

### Swap

Have some. Agent runs and Go builds are memory-hungry and arrive in bursts, and
the process the kernel picks when memory runs out should not be the control
plane.

## Packages

- `qemu-kvm` is not needed; `/dev/kvm` is. On bare metal it is there.
- `debootstrap`, `e2fsprogs` -- building the golden rootfs.
- `bridge-utils` or just `iproute2`, plus `nftables`. The Firecracker config
  names a bridge (`fcbr0`) and a guest network (`172.30.0.0/16`); guests need
  NAT out and `net.ipv4.ip_forward=1`.
- `postgresql` and `postgresql-client`. The data block shells out to `pg_dump`
  and `pg_restore`, and a client older than the server refuses to run, so pin
  both. `config.data.snapshot_dir` and the client's `bin_dir` must agree with
  the version actually installed. Debian 13 ships Postgres 17; the PGDG archive
  is the right move if a specific major version is wanted, because the version
  is awkward to change once there is data.
- `chromium`. The verifier drives a real browser over the DevTools protocol and
  finds it by looking for the binary and reading `DevToolsActivePort`. Headless,
  `--no-sandbox` is already handled.
- A Go 1.25 toolchain, from upstream rather than the archive. Needed on the host
  to build dabberzd, and needed *inside the golden image* because an app's build
  step runs there -- Forms is a Go module and its `build.command` is `go build`.
- `git`. Same: on the host for the bare repositories, in the image for
  checkouts.
- The `claude` binary in the golden image, which is what the agent is.
- `systemd-timesyncd` or equivalent. Clerk's tokens are time-sensitive and a
  skewed clock reads as a forged credential.

## DNS, and why it is two names

The control plane and the previews must not share a registrable domain, and the
code enforces it: a hostname inside the preview zone that has no route is a dead
end rather than falling through to the panel. If they shared a domain, the panel
-- which has a shell behind it -- would be reachable unauthenticated at any name
under the preview zone.

- One name for the control plane, locked down by whatever sits in front.
- A wildcard for the preview zone, deliberately not locked down, because that is
  where customers' apps answer.

Both resolve to the same listener; the daemon routes by `Host`. That listener
needs 80 and 443, not a high port: a preview URL is `http://host` with no port,
so a control plane on 8090 produces previews that resolve and then refuse to
connect. This is the one thing that bit during local validation.

TLS has two shapes and both are supported. A CDN in front terminates TLS and the
origin needs no wildcard certificate, which is why the in-process router exists
at all. Without a CDN, Caddy is still the right router, because a wildcard
certificate can only be issued over DNS-01.

## Backups

Not yet wired, and worth saying what has to be in one:

- the control plane SQLite file -- without it the Postgres databases are
  anonymous
- the Postgres cluster
- the bare repositories, which hold every installation's actual code and every
  change ever promoted
- the master key, without which the secret vault is noise

The workspaces are not in that list. They are derived from the repositories and
can be rebuilt.
