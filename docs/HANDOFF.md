# Handoff: dbz-alpha, and what to do next

For an agent picking this up with SSH access to the host. Updated at the end of
the first session that had it, on branch `claude/trusting-brown-dns9i5`; the
host runs that branch's head. Verify rather than trust what follows — it was
accurate when written and the machine has moved on.

From Windows the SSH alias lives in the Debian WSL distro, not Git Bash:
`wsl.exe -d Debian -- ssh dbz-alpha`. `/opt/dabberz/src` is root-only, so work
there under `sudo`.

## What dabberz is

SaaS-as-a-service: **the self-host for people who are not technical.** A
customer installs their own HubSpot or Notion equivalent, dabberz runs it, an
agent changes it in English, and the data stays theirs. The thing being sold is
operational confidence rather than software — *"don't make running your tool
distract you from your core business."*

Pricing is never per seat and never per gigabyte; the customer brings their own
Claude subscription; agent access is free in every app, which is the antithesis
of "seats plus extra for agents."

Read in this order: `DIRECTION.md` (why), `PLATFORM.md` (the contract apps are
built against), `HOST.md` (this machine).

## The machine

`dbz-alpha`, OVH KS-LE-5. 12 cores, 62 GiB, two 2 TB spinning SATA disks.
Public address `147.135.70.81`.

- Debian 13, bootstrapped by `deploy/bootstrap-host.sh` (idempotent, safe to
  re-run; `--storage` is the only destructive part and it is already done).
- ZFS pool `tank`, mirrored across `sda5` and `sdb6`, ~1.65 TiB usable.
  Datasets: `tank/postgres` (8k recordsize), `tank/dabberz/{data,vm,snapshots}`.
- Postgres 17 on `/srv/pgdata`, `full_page_writes` off. The daemon's admin
  role `dabberz` is CREATEROLE/CREATEDB, not superuser.
- `dabberzd` runs under systemd from `/opt/dabberz/bin/dabberzd`, as root with
  `UMask=0027`. Config `/etc/dabberz/dabberz.yaml`, secrets
  `/etc/dabberz/dabberzd.env`, source `/opt/dabberz/src`. Every edit made to
  either file this session left a dated `.bak-*` beside it.
- **Everything inside an instance runs as `dabberz`**, not root
  (`driver.local.run_as`): the agent, builds, customers' services, shells. Each
  instance has its own `home/` beside its `workspace/`.
- Both domains are proxied by Cloudflare. `ondabberz.link` is on Flexible and
  works over HTTPS. `panel.dabberz.com` answers only over plain HTTP (HTTPS is
  a 521) — see below.
- Ports 80 and 443 accept only Cloudflare's ranges and the host itself
  (`/etc/nftables.d/dabberz-edge.nft`, from the bootstrap's `firewall` step).
  SSH is untouched by it.
- Mail and storage are **stand-ins** on loopback (`deploy/standins/`): aiosmtpd
  writes every message to `journalctl -u dabberz-mail-standin` and delivers
  nothing; versitygw serves S3 over `/tank/dabberz/data/objects`. The Linode
  bucket's values are kept as comments in the YAML for when storage is real.

### Deploying a change

Push the branch, then on the host as root: `git merge --ff-only` in
`/opt/dabberz/src`, `make build`, install each binary as `*.new` and `mv` it
over the old one, restart. The restart stops every customer's service; the
daemon brings back every workspace recorded as running once it is listening,
which takes a second or two for `intake`. Look for `workspace resumed` in
`journalctl -u dabberzd`, and `did not come back` for one that failed.

## Known wrong, in priority order

1. **`panel.dabberz.com` has no TLS at the origin.** Cloudflare proxies it and
   expects 443, which nothing serves, so HTTPS is a 521 and only plain HTTP
   works. Do not sign in over it. The daemon does not terminate TLS itself, so
   this needs a Cloudflare origin certificate plus something on 443 (a TLS
   listener in the daemon, or a terminator in front), then Full (strict).
2. **The Clerk secret should be rotated** — it appeared in a chat transcript.
   Rotate in Clerk, then update `CLERK_SECRET_KEY` in the env file. The name
   is now right; that it is read was not confirmed end to end, because only a
   new user's first sign-in exercises it.
3. **Failed installations cannot be deleted or retried**, and keep their name.
   `recruiting` and `hiring` are two such, from this session; `hiring` also
   still holds a provisioned database. Its machine was moved aside to
   `/tank/dabberz/data/vms-released/` before the release fix existed.
4. **The install tests leak the services they start.** A run of
   `./internal/install` left ~50 `forms serve` processes behind as the test
   user. Kill them after running it on the host: `pkill -u $USER -f './forms serve'`.
5. **A `scratch` platform token** created on 2026-11-18 is live and was not
   made by this session. Leave it unless its owner says otherwise.
6. **golangci-lint is not installed on the host**, so nothing this session
   wrote has been linted.

## What this session fixed, all running on the host

Each was found by working the list or running the loop. Every code fix has a
test that was seen to fail with the fix removed; the first two were changes to
the host, checked on the host.

- The Clerk secret was under a name nothing reads; renamed in the env file.
- Port 80 was open to anyone; now Cloudflare's ranges only.
- Storage with a bucket but no secret key counted as provided.
- Provisioning failed on Postgres 16+ for a non-superuser admin: `CREATE
  DATABASE ... OWNER` needs SET on the role. The database tests now always run
  as a non-superuser made from whatever DSN they are given.
- `go` was only on PATH for login shells, so the daemon could not build apps.
- Nothing ever destroyed a machine, so finished changes and failed installs
  held capacity forever; with 10 vCPUs that meant one change, ever.
- The agent ran as root, where Claude Code rightly refuses to skip its
  permission prompts. Now `dabberz`, with the repositories owned to match.
- The control plane's database was world-readable; the unit sets a umask.
- With a fresh home the agent had no git identity; it commits as
  `dabberz agent <agent@dabberz.invalid>`.
- The browser check saw an app's signed-in surface as a stranger, so a page
  behind sign-in could never pass. It now looks as the change's owner, with a
  **preview pass** (`internal/previewpass`): an HMAC over one preview host, one
  user and a 15-minute expiry, keyed per daemon process, set by the harness as
  a host-only HttpOnly cookie, accepted by the edge only on that host and
  stripped before the app sees it. The owner's platform token was the
  obvious alternative and the wrong one: it is the whole account, handed to a
  page an agent just wrote.
- The repositories directory was handed to `dabberz` but left 0700, so the
  first promote could not reach the repository it was pushing to.
- A daemon restart took every customer's site down until somebody restarted
  each workspace by hand. The daemon now resumes every workspace recorded as
  running, leaving each checkout where it stands, so a rollback survives a
  restart. Proven on the host by restarting the daemon and touching nothing:
  `intake` answered within two seconds, on its rolled-back revision, with
  every response intact.

## The loop, on hardware

Run end to end on dbz-alpha, against a real agent, real Postgres, a real
Chromium and Cloudflare in front:

```
export DABBERZ_TOKEN=$(cat ~/.dabberz-token)   # the `milestone` token, on the host

# 1. install         POST /v1/installations                  "intake", running
# 2. open a change   POST /v1/installations/{id}/changes     3s, own preview and data fork
# 3. drive agent     POST /v1/workspaces/{ws}/messages       15s; committed 8aad53b
# 4. check           POST /v1/changes/{id}/check             suite + browser as owner, ready
# 5. promote         POST /v1/changes/{id}/promote           live on 8aad53b, machine released
# 6. roll back       POST /v1/installations/{id}/rollback    back on e7dd585
```

What was checked rather than assumed: production held a form and a real
response before the change; after the promote its thank-you page carried the
agent's new link and every response was still there; after the rollback the
link was gone and the responses still were. `intake` is left running on
`e7dd585`, with its `Job application` form and three responses.

To exercise an app's signed-in surface from the host without a browser, the
edge accepts the platform token as a visitor. Send it to `127.0.0.1` with the
preview's `Host` header, never through Cloudflare: Flexible means the hop
from Cloudflare to this machine is plain HTTP.

The agent said 7 of Forms' own tests skip in its workspace for want of
`FORMS_TEST_DATABASE_URL`; the check's suite, which has the database, passed.

## The next milestone

The first of the known-wrong items: TLS at the origin for `panel.dabberz.com`.
Until it exists the owner cannot sign in to the panel safely, and the panel is
where a person rather than a curl command drives the loop above. After that,
the two gaps below, which are what stand between this and a second account.

## Two structural gaps, both blocking a second account

Neither matters while the owner is the only user. Both are written up as
tasks and neither is cosmetic.

- **The Firecracker driver does not boot guests.** `Create`, `Exec`, `Destroy`
  and `OpenSession` return `vm.ErrNotSupported`, so the only working driver is
  `local`, which runs agents and customers' services on the host -- now as
  one shared unprivileged account rather than as root, but with nothing
  between one instance and another. The address layout, slot accounting and
  machine config already exist. **Hard constraint: the guest rootfs must be a copy-on-write
  overlay of the golden image, never a copy** — a 40 GiB copy per fork on these
  spinning disks is minutes before an agent can start.
- **The agent's Claude token is process-wide.** `CLAUDE_CODE_OAUTH_TOKEN` is
  read once from the daemon's environment, so one subscription funds every
  agent run. That contradicts the whole bring-your-own position. It needs the
  shape `internal/gitauth` already uses: resolved per workspace from that
  customer's own stored credential.

## How this codebase expects to be worked on

- **Run the thing, do not just test it.** Nearly every real bug in this project
  was found by executing against real Postgres, real Chromium and real git
  rather than by stubs. Several were found by a human pasting terminal output.
- **Prove a guard by removing it.** A test that has never been seen to fail is
  not evidence. Disable the check, watch the test fail, put it back.
- **Test the fragment in its own context.** A shell line checked without the
  script's `set -euo pipefail` is not checked: that exact mistake hid a silent
  `exit 141` on a `tr | head` pipeline that stopped the bootstrap three times.
- **Comments say why, not what.** Match the surrounding prose style.
- Branch `claude/trusting-brown-dns9i5`. Commits end with the Co-Authored-By
  and Claude-Session trailers used throughout the history; never put a model
  name in a commit, PR or code comment.
- `make build`, `go test ./...`, `golangci-lint run ./...`. Postgres-backed
  tests need `DABBERZ_TEST_CLUSTER_DSN` and `DABBERZ_TEST_PG_BIN`.

## Traps this project has already fallen into

Each of these cost real time and all are now guarded or documented. They are
listed because they rhyme with mistakes still available to make.

- **Installing over a running binary** fails with `ETXTBSY`. Build, then
  rename — a rename leaves the running process on its original inode.
- **Env var names drifting between docs and code.** Three were wrong in one
  deployment. A credential under a name nothing reads is indistinguishable
  from one never set. There is now a test that scans the deployment files for
  anything shaped like one and fails unless the code reads it.
- **Collaborators wired in tests but not in `main.go`.** Three separate
  outages came from this: the database URL, the identity assertion at the
  edge, and the browser verifier — the last of which made a check's *absence*
  read as a pass.
- **A quiet stream looks like a working one.** The activity feed wrote nothing
  when nothing happened, which any proxy kills after ~100 seconds; the client
  then waits on a socket nobody will write to.
- **Tests that substitute the wrong seam.** Several tenancy tests swapped the
  authenticator while the code had moved to a translator, so they passed
  requests they existed to refuse.
