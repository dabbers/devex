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
over the old one, restart. **A restart takes every customer's site down** —
see the first item below — so follow it with a stop and start of each running
workspace (`POST /v1/workspaces/{id}/stop`, then `/start`).

## Known wrong, in priority order

1. **Restarting the daemon kills every service it started, and nothing brings
   them back.** Services are `setsid`-detached, but systemd stops the whole
   control group, and there is no startup reconciliation. So every deploy and
   every reboot leaves each installation answering 502 until somebody restarts
   its workspace by hand. The fix wants a pass at startup that brings up every
   workspace recorded as running; changing `KillMode` alone would survive a
   deploy but not a reboot.
2. **The browser check cannot pass for an app whose front page needs sign-in,
   so no Forms change can be promoted.** `dabberz-verify` begins with an
   anonymous GET of the preview root and stops there; Forms answers `/` with
   401, correctly. Either the verifier sees the preview as the change's owner
   (a short-lived identity assertion scoped to that one preview host), or the
   manifest names a public entry point to check. That is a design decision,
   not a bug fix, and it is where the milestone loop stopped.
3. **`panel.dabberz.com` has no TLS at the origin.** Cloudflare proxies it and
   expects 443, which nothing serves, so HTTPS is a 521 and only plain HTTP
   works. Do not sign in over it. The daemon does not terminate TLS itself, so
   this needs a Cloudflare origin certificate plus something on 443 (a TLS
   listener in the daemon, or a terminator in front), then Full (strict).
4. **The Clerk secret should be rotated** — it appeared in a chat transcript.
   Rotate in Clerk, then update `CLERK_SECRET_KEY` in the env file. The name
   is now right; that it is read was not confirmed end to end, because only a
   new user's first sign-in exercises it.
5. **Failed installations cannot be deleted or retried**, and keep their name.
   `recruiting` and `hiring` are two such, from this session; `hiring` also
   still holds a provisioned database. Its machine was moved aside to
   `/tank/dabberz/data/vms-released/` before the release fix existed.
6. **The install tests leak the services they start.** A run of
   `./internal/install` left ~50 `forms serve` processes behind as the test
   user. Kill them after running it on the host: `pkill -u $USER -f './forms serve'`.
7. **A `scratch` platform token** created on 2026-11-18 is live and was not
   made by this session. Leave it unless its owner says otherwise.
8. **golangci-lint is not installed on the host**, so nothing this session
   wrote has been linted.

## What this session fixed, all running on the host

Each was found by running the loop, and each has a test that was seen to fail
with the fix removed.

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

## The next milestone

The loop has now run on hardware as far as step 4:

```
export DABBERZ_TOKEN=$(cat ~/.dabberz-token)   # the `milestone` token, on the host

# 1. install                         ok   POST /v1/installations          -> "intake", running
# 2. open a change                   ok   POST /v1/installations/{id}/changes   (3s)
# 3. drive the agent                 ok   POST /v1/workspaces/{ws}/messages     (15s, committed)
# 4. check                           blocked: suite passed, browser check 401 (item 2)
# 5. promote                         not reached
# 6. roll back                       not reached
```

The live pieces: installation `ins_06gz2241nb4349s4v42ej8bpx0` (`intake`), change
`chg_06gz23m4fnnpd006dfbe7jadtg` with the agent's commit `8aad53b`, whose
preview is `https://web-forms-intake-06gz23m4fnnpd006dfbe7jadtg.ondabberz.link`.
Once item 2 is decided, re-run the check on that change and carry on from 5.

The agent's reply on step 3 said 7 of Forms' own tests skip in its workspace
for want of `FORMS_TEST_DATABASE_URL`; the check's suite, which has the
database, passed.

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
