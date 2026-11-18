# Handoff: dbz-alpha, and what to do next

For an agent picking this up with SSH access to the host. Written at commit
`cd25e2a` on branch `claude/trusting-brown-dns9i5`.

The previous session had no access to the machine, so every step was a command
described in chat and an output pasted back. That is why this document exists:
you can do in ten minutes what took a day that way. Verify rather than trust
what follows — it was accurate when written and the machine has moved on.

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
- Postgres 17, data directory moved to `/srv/pgdata` on the pool. Confirmed.
- `dabberzd` runs under systemd from `/opt/dabberz/bin/dabberzd`, config
  `/etc/dabberz/dabberz.yaml`, source checkout `/opt/dabberz/src`.
- DNS: `panel.dabberz.com` → the host, direct. `*.ondabberz.link` → Cloudflare,
  proxied. Two different registrable domains, which the code requires.

### Verified working

Control plane listening on `0.0.0.0:80`, Clerk authenticating, `data block
ready`, `catalog ready apps=1`, and platform-token auth end to end —
`dabberzctl status` returns capacity JSON.

### Known wrong or unverified, in priority order

1. **`CLERK_SECRET_KEY` is probably under the wrong name in
   `/etc/dabberz/dabberzd.env`.** The previous session said
   `DABBERZ_CLERK_SECRET`, which nothing reads. Sign-in still works (verified
   against the issuer's public keys) but new-user email attribution does not.
   `grep -c CLERK_SECRET_KEY /etc/dabberz/dabberzd.env` — zero means fix it.
2. **`blocks.mail_url` and `blocks.storage` are empty**, so Forms cannot
   install: its manifest declares both and the installer refuses rather than
   installing something that fails at runtime. Needs any reachable SMTP URL and
   an S3-compatible endpoint. The secret key is `DABBERZ_STORAGE_SECRET_KEY` in
   the env file and has no YAML field at all.
3. **`full_page_writes` was never confirmed off.** It is safe now that Postgres
   is on copy-on-write storage, and on spinning disks it is worth real latency.
   `sudo -u postgres psql -tAc 'show full_page_writes'`.
4. **Port 80 is not firewalled to Cloudflare's ranges.** The daemon answers
   preview hostnames over plain HTTP to anyone who hits the IP directly with
   the right `Host` header, which bypasses the CDN entirely.
5. **`panel.dabberz.com` has no TLS.** The nameserver transfer to Cloudflare
   was in progress. Do not sign in over plain HTTP. When proxying it, use an
   origin certificate with Full (strict) rather than Flexible: that hop carries
   a root shell, the secrets vault and the audit trail.
6. **The Clerk secret should be rotated** — it appeared in a chat transcript.
7. **An old token is still live**: `tok_06gz0nj2c5w0zn9wp78zzv8wac`, never
   used. `dabberzctl token revoke` it.
8. Old Postgres data may still sit at `/var/lib/postgresql/17/main`, holding
   ~100 MB of the 97 GiB root for nothing.

## The next milestone

Nothing in the product loop has ever run on hardware. Everything below has been
proven in a sandbox against real Postgres, real Chromium and real git, and
never on this machine:

```
export DABBERZ_TOKEN=$(dabberzctl token create -name "cli" | head -1)

dabberzctl status
curl -s localhost/v1/apps -H "Authorization: Bearer $DABBERZ_TOKEN"

# 1. install
curl -s -X POST localhost/v1/installations -H "Authorization: Bearer $DABBERZ_TOKEN" \
  -H 'content-type: application/json' -d '{"app":"forms","name":"recruiting"}'

# 2. open a change against it            POST /v1/installations/{id}/changes
# 3. drive the agent into the change     POST /v1/workspaces/{change.workspace_id}/messages
# 4. check it                            POST /v1/changes/{id}/check
# 5. promote                             POST /v1/changes/{id}/promote
# 6. roll back                           POST /v1/installations/{id}/rollback
```

Step 3 is the one that has never been exercised with a real agent doing real
work on this machine. The agent acts without asking (`permission_mode:
bypassPermissions`), which is safe because it works on its own branch, against
a forked copy of the data, behind a preview nobody depends on, and a human
promotes.

Expect the browser check in step 4 to need `*.ondabberz.link` resolving to the
host and reachable over the preview hostname, because the verifier drives a
real Chromium at the public address rather than at a port.

## Two structural gaps, both blocking a second account

Neither matters while the owner is the only user. Both are written up as
tasks and neither is cosmetic.

- **The Firecracker driver does not boot guests.** `Create`, `Exec`, `Destroy`
  and `OpenSession` return `vm.ErrNotSupported`, so the only working driver is
  `local`, which runs agents and customers' services on the host as the daemon
  process, as root. The address layout, slot accounting and machine config
  already exist. **Hard constraint: the guest rootfs must be a copy-on-write
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
