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

So: a small root filesystem, and the rest left unallocated.

The machine is a KS-LE-5: two 2000 GB SATA disks presented as JBOD, 3.6 TiB
in total. The installer's default plan spends all of it on a root filesystem --
which would put the OS, Postgres, every workspace checkout, every guest rootfs
and an unpruned snapshot directory on one volume, where a runaway build takes
down sshd along with the daemon. What was asked for instead, and what the
machine was built with:

| | Usable | FS | RAID |
|---|---|---|---|
| `/boot` | 1.0 GiB | ext4 | RAID1 |
| `/` | 97.7 GiB | ext4 | RAID1 |
| `swap` | 2 x 7.8 GiB | swap | none |
| unallocated | 3.4 TiB | -- | -- |

JBOD matters as much as the sizes. The disks are presented individually, which
is what software RAID1 needs now and what a pool needs later; hardware RAID
would have obstructed both.

Two things that were traded away deliberately, recorded so they are decisions
rather than oversights:

- **Swap is not mirrored.** Which makes the redundancy asymmetric: `/boot` and
  `/` survive losing a disk, and then the machine probably dies anyway because
  it was swapping onto the one that went. Acceptable on a host that is
  rebuildable; mirror it on one that is not, at half the swap.
- **The disks are spinning, not flash.** Fine for validating. The first thing
  to hurt will be Postgres WAL `fsync` latency, and a copy-on-write pool makes
  sync writes exactly the operation it is worst at. The fix is flash for the
  WAL rather than a different layout, so it is an argument about the next
  machine, not this one.

Then `zpool create tank mirror <part4> <part4>` later, with everything dabberz
on datasets there. OS redundancy and a mirrored copy-on-write pool, which beats
having either one alone.

100 GB of root is deliberate rather than stingy. The toolchain, Chromium and
packages fit easily; the things that grow -- the golden rootfs, the per-guest
copies under `chroot_base` where each fork's `disk_gib` is actually spent,
Postgres, snapshots -- belong on the pool.

If the installer refuses to leave `/` short of the whole disk, partition one
disk and leave the second untouched. That loses OS redundancy and keeps a clean
whole device for the pool, which ZFS prefers anyway. It is the fallback, not
the first choice.

Root stays ext4. ZFS on root is harder to recover and buys nothing here; ZFS is
wanted for the data pool only.

A caveat, so the pool is not mistaken for a finished plan: `zfs clone` copies a
dataset, so forking a cluster that way means a second postmaster per change on
its own port. Far cheaper than `pg_dump` at size, and a real design change
rather than a drop-in. Neon-style storage is the other branch. The space is the
prerequisite either way; the mechanism is still open.

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

Have some, and set the size rather than taking the installer's default, which
tends to be small. Agent runs and Go builds are memory-hungry and arrive in
bursts, and the process the kernel picks when memory runs out should not be the
control plane.

### An SSH key, at install time

Leave the key field empty and the provider emails a root password: a credential
that arrives over email and then lives in a mailbox, for a machine that will
hold other people's data.

## Getting the script onto the box

A fresh install has no git -- that is one of the things being installed -- so
the bootstrap arrives as a single file over HTTP rather than as a clone:

```
apt-get update && apt-get install -y curl
curl -fsSL -o bootstrap-host.sh \
  https://raw.githubusercontent.com/dabbers/devex/refs/heads/claude/trusting-brown-dns9i5/deploy/bootstrap-host.sh
less bootstrap-host.sh
bash bootstrap-host.sh
```

Read before run, deliberately. The alternative spelling of those four lines is
`curl ... | bash`, which asks somebody to trust a URL on a machine that is
about to hold other people's data, and saves them one command for it.

Note the branch in that URL, and in the clone the script makes. The default
branch is a long way behind; cloning it would produce a tree with none of this
in it.

And check what arrived, because a branch URL on raw.githubusercontent.com is
served from a cache that lags a push by minutes and ignores query strings:

```
grep -c REPO_BRANCH bootstrap-host.sh   # 3; zero means a stale copy
```

A stale copy is not obviously broken, which is what makes it worth checking --
it would run, and clone the wrong branch. If the count is zero, either wait and
fetch again or use a URL pinned to a commit, which is never served stale:

```
https://raw.githubusercontent.com/dabbers/devex/<commit-sha>/deploy/bootstrap-host.sh
```

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

## After the bootstrap

The script stops at a working host. Four things remain, and the first is worth
doing before anything else touches the database.

### Move Postgres onto the pool, while it is still empty

The package put the cluster on the root filesystem. It needs to be on
`tank/postgres`, which was created with an 8k recordsize for it. Doing this
now costs a restart. Doing it after a customer's data is in there costs a dump,
a restore and an outage.

```
systemctl stop postgresql@17-main
zfs set mountpoint=/srv/pgdata tank/postgres
rsync -aXS --numeric-ids /var/lib/postgresql/17/main/ /srv/pgdata/
chown -R postgres:postgres /srv/pgdata
# in /etc/postgresql/17/main/postgresql.conf:
#   data_directory = '/srv/pgdata'
systemctl start postgresql@17-main
```

Then check it actually moved, rather than assuming:

```
sudo -u postgres psql -tAc 'show data_directory'
```

While in that file, one setting is worth changing for this storage:

```
full_page_writes = off
```

Postgres writes whole pages after a checkpoint to protect against a torn write
-- half a page on disk because the power went out mid-write. On a
copy-on-write pool that cannot happen, because a block is never overwritten in
place. Leaving it on means writing every page twice for a hazard the storage
has already removed, which on spinning disks is the expensive kind of
redundant.

### The rest

- **The config.** `configs/dabberz.example.yaml` is the annotated one. The
  master key comes from `dabberzctl keygen` and belongs in the environment,
  never the file. The data block's DSN is already in `/etc/dabberz/data.env`.
- **The binaries**, which nothing has built yet:

  ```
  cd /opt/dabberz/src && make build
  install -d /opt/dabberz/bin
  install -m0755 bin/dabberzd       /opt/dabberz/bin/dabberzd.new
  install -m0755 bin/dabberzctl     /opt/dabberz/bin/
  install -m0755 bin/dabberz-verify /usr/local/bin/
  mv /opt/dabberz/bin/dabberzd.new  /opt/dabberz/bin/dabberzd
  ```

  The build-then-rename is not ceremony. Writing over the binary of a running
  process fails outright on Linux -- "text file busy" -- and a rename does not,
  because the running process keeps the inode it started from. The same
  mistake inside the platform is why deploying a change stops a service before
  rebuilding it.

  `dabberz-verify` goes somewhere on `PATH`, because the daemon runs it by
  name on the machine that drives the browser.
- **The systemd unit** is `deploy/dabberzd.service`, which reads that env file:

  ```
  install -m0644 deploy/dabberzd.service /etc/systemd/system/
  systemctl daemon-reload && systemctl enable --now dabberzd
  journalctl -u dabberzd -f
  ```
- **DNS and TLS**, below.

### Identity is not optional on this machine

The one configuration that will not announce itself. `auth.kind: single`
treats every request as the owner without asking for anything, which is right
for a daemon on a loopback address and catastrophic on a public one: whoever
reaches the port has a shell, the secrets vault and the whole audit trail, and
nothing about the deployment looks wrong.

The daemon refuses to start in that combination rather than warning about it,
because a warning scrolls past and the thing it is protecting is a root shell
on the internet. Two ways out, both configuration:

- bind `server.addr` to `127.0.0.1` and reach the panel through an SSH tunnel,
  which is the right shape for a first bring-up; or
- configure a provider, which is the right shape for anything after that.

A credential that has been in a chat log, a terminal transcript or a
screenshot is not a credential any more. Rotate before this host uses it.

## DNS, and why it is two names

The control plane and the previews must not share a registrable domain, and the
code enforces it: a hostname inside the preview zone that has no route is a dead
end rather than falling through to the panel. If they shared a domain, the panel
-- which has a shell behind it -- would be reachable unauthenticated at any name
under the preview zone.

- One name for the control plane, locked down by whatever sits in front.
- A wildcard for the preview zone, deliberately not locked down, because that is
  where customers' apps answer.

On this deployment that is `panel.dabberz.com` for the control plane and
`*.ondabberz.link` for previews. Different registrable domains, which is the
part that matters: the separation is what stops the panel being reachable
under a preview name.

The two are fronted differently, and the difference decides how TLS works.
The preview zone is proxied by a CDN, so TLS terminates there and the origin
serves plain HTTP -- which is the whole reason the in-process router exists
instead of Caddy. The control plane resolves straight to the host, so nothing
terminates TLS for it yet.

Putting the control plane behind a CDN also puts one in front of two things
that are not request-and-response: the workspace shell is a WebSocket and the
activity feed is server-sent events. Both are long-lived and both are the
first casualties of anything that drops idle connections. The feed now emits a
keepalive comment rather than going silent between events, which is what makes
it survive a proxy; a shell with nobody typing is the other case to watch.

Which is a decision rather than an oversight, and worth making deliberately.
Putting the control plane behind the same CDN in its most permissive mode
would leave the hop from the CDN to the origin unencrypted across the public
internet -- acceptable for a form somebody is filling in, not for the surface
with a shell, a secrets vault and the audit trail behind it. So either an
origin certificate with strict verification, or terminate it on the host and
leave it off the CDN.

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
