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
- Both domains are proxied by Cloudflare, both zones on Flexible with Always
  Use HTTPS, so a visitor's plain HTTP gets a 301 at Cloudflare's edge.
  Flexible means the hop from Cloudflare to this machine is plain HTTP — see
  below. Never redirect to HTTPS at the origin while that is so: the origin
  sees every request as HTTP and would loop.
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

1. **Nothing has TLS at the origin.** Visitors reach Cloudflare over HTTPS,
   but Cloudflare reaches this machine over plain HTTP across the internet, so
   panel sign-in cookies cross that hop in the clear. The daemon does not
   terminate TLS itself, so this needs a Cloudflare origin certificate plus
   something on 443 (a TLS listener in the daemon, or a terminator in front),
   then Full (strict) on both zones, port 80 closed in the edge firewall, and
   only then HSTS. The owner chose to defer it.
2. **Clerk is a development instance** — the sign-in card carries the orange
   "Development mode" badge, so the keys are `pk_test_`/`sk_test_`. Dev
   instances share OAuth credentials and cannot vend provider tokens, which
   this project already met as
   `oauth_shared_credentials_token_retrieval_not_supported`. Per-account
   GitHub linking therefore cannot work as configured, whatever else is fixed;
   it needs a production instance.
3. **The Clerk secret should be rotated** — it appeared in a chat transcript.
   Rotate in Clerk, then update `CLERK_SECRET_KEY` in the env file. The name
   is now right; that it is read was not confirmed end to end, because only a
   new user's first sign-in exercises it.
4. **`ondabberz.link` is not a public suffix.** Until it is, a browser treats
   every customer's preview as the same site: `SameSite` does nothing
   between them, and a page's script can write a cookie for all of them.
   The edge already keeps cookies an app's *server* sets to their own
   hostname; script it cannot reach. Listing needs a PR to
   `publicsuffix/list` (private section), a `_psl` TXT record pointing at
   it, and the domain registered well ahead; browsers pick it up over
   weeks to months. The owner's to do.
5. **A `scratch` platform token** created on 2026-11-18 is live and was not
   made by this session. Leave it unless its owner says otherwise.
6. **golangci-lint is not installed on the host**, so nothing this session
   wrote has been linted.
8. **A rollback leaves the serving branch ahead of what is live.** Rollback
   redeploys the previous revision but leaves `main` on the rolled-back
   commit, and new changes branch from `main`, so the next promote ships the
   rolled-back commit again. Older than concurrent changes, but they make it
   likelier to bite. Rollback should move the branch, or changes should start
   from the installation's `revision`.

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
- A failed installation kept its name, its repository and sometimes its
  database for good. `DELETE /v1/installations/{id}` now removes all of it,
  and only for one that failed: a running installation has held a
  customer's data, and deleting that wants an export and a confirmation
  first. Deleting frees the name, so installing again is the retry.
  `hiring` and `recruiting` were deleted with it on the host, along with the
  machine moved aside by hand; only `intake` is left. Deleting releases every
  workspace on the repository, not only the installation's own, since all
  their records go with it.
- An agent restyling a change killed its service to restart it and could not
  start it again: services run with credentials only the platform has. The
  agent is now told never to touch them, and the platform rebuilds and
  restarts a workspace's services after every agent turn.
- Rolling back left the branch at the rolled-back commit, so the next change
  carried it and approving that change would have put it live again. Rollback
  now moves the branch with the deploy and keeps the old commit under
  `refs/dabberz/rolled-back/`. `intake`'s branch was moved by hand to match;
  its open GeoCities change was opened before this and still contains the
  rolled-back thank-you link (`8aad53b`).
- Any installation could set a cookie for every other one, since all
  previews are siblings under one domain. The edge now strips `Domain` from
  every `Set-Cookie` an app sends, so each cookie stays on its own hostname
  and keeps every other attribute. The test runs a real cookie jar across
  two installations and fails with the rewrite removed.

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

## Several changes at once

An installation can have several changes open together ("add a checkout
page", "add PostHog tracking"), each with its own branch, machine, preview and
copy of the data. Not run on the host yet: proven by the tests in
`internal/install/concurrent_test.go`, against real git and real Postgres.

- **Capacity.** Opening asks the driver for room first (`Workspaces.Room`). A
  host with no room answers **503** and nothing is made — no row, no copy of the
  data. Two opens racing for the last slot: the driver refuses the loser's
  machine, and that change is released and recorded `abandoned`, not left
  `blocked` holding a database.
- **Order is the customer's.** Each `promote` call means "this one next". One
  installation's promotes queue behind each other in-process, so the second
  always starts from what the first left live.
- **Catching up is a merge, not a rebase.** When production is not already in
  the change's branch, production's revision is merged into it in the change's
  workspace. A rebase would rewrite the commits the checks and the agent's
  conversation refer to, and replay them one at a time, so one overlap can
  conflict several times; a merge is one commit with production as a parent,
  so the push stays a fast-forward.
- **Clean merge:** the change's copy of the data is replaced by a fresh copy of
  production's, then the change is checked again (suite + browser) on the
  merged commit, and only then goes live. The fresh copy is the point: the
  preview restarts on it, so the merged code's migrations meet production's
  current rows there before they meet production.
- **Conflict:** nothing goes live. The merge is aborted, so the workspace is
  exactly as the agent left it; the change becomes `conflicted` with
  `conflicts: ["README.md", ...]`. `POST /v1/changes/{id}/resolve` sends that
  change's own agent a message saying what went live meanwhile, which files
  conflict, and to `git merge <production revision>`, keep both, test and
  commit. It is an ordinary turn on the customer's subscription. Then `check`
  (which accepts `conflicted` and clears `conflicts` once production is in the
  branch) and `promote` as usual.
- **Data is never copied back.** Promote deploys code only; the app migrates
  on restart against production's own data. Uncommitted tracked edits in a
  change refuse the promote (`409`), since they would not go live and a merge
  would sweep them into a commit nobody checked.

### API, for the panel

| Call | Body | Answer |
|---|---|---|
| `POST /v1/installations/{id}/changes` | `{"title": "Add a checkout page"}` | `201 {"change": Change}`; `503 {"error"}` when the host is full; `409 {"change", "error"}` when it was made but failed to start |
| `GET /v1/installations/{id}/changes` | — | `200 {"changes": [Change]}`, newest first; open ones have a state below other than `promoted`/`abandoned` |
| `GET /v1/changes/{id}` | — | `200 {"change": Change}` |
| `POST /v1/changes/{id}/check` | — | `200 {"change": Change}`; state `ready` or `blocked`. Accepted from `open`, `ready`, `blocked`, `conflicted` |
| `POST /v1/changes/{id}/promote` | — | `200 {"installation": Installation}`; `409 {"change": Change, "error"}` when refused — the change's `state` says why: `conflicted` (see `conflicts`), `blocked` (re-check after catching up failed), or unchanged (not ready, unchecked commits, uncommitted edits). Synchronous, and minutes long when it has to catch up and re-check |
| `POST /v1/changes/{id}/resolve` | — | `202 {"message", "reply", "change"}` like `POST /v1/workspaces/{ws}/messages`; the reply streams on that workspace. `409` unless the change is `conflicted`, or if its agent is already busy |
| `DELETE /v1/changes/{id}` | — | `204`; abandons it and frees its machine and data |

`Change` gains `base_revision` (the commit it started from; production being
elsewhere means another change went live meanwhile — compare with the
installation's `revision`) and `conflicts` (file paths), and its `state` gains
`conflicted`.

## Checking it from outside, without SSH

A sandboxed agent's `curl` is rewritten by its own egress proxy and returns
synthetic errors wearing the server's clothes — a previous session concluded
from them that the host was unreachable, which was wrong. A real Chromium goes
straight through, so Playwright is the instrument for looking at this
deployment from somewhere that is not the host.

What it reaches with no credential, which is most of what matters: the panel's
sign-in page, a preview hostname with nothing behind it (404 from the mux
rather than a CDN error means Cloudflare is reaching the origin), and an
installed app's **public** surface, which is deliberately anonymous — so a
form being filled in can be checked end to end from outside. Signing in is the
exception and stays with the owner.

## The panel drives the loop

`panel.dabberz.com` opens on **Services**: the catalog and a deploy form
(name only; custom domains and bring-your-own apps are shown as coming), a
page per service with its address, version, rollback and a box that opens a
change and hands the prompt to the agent, and a page per change with the
agent's chat, the live preview, a shell, the checks, and approve-and-go-live,
enabled only once its checks pass. The older Repositories / New project
screens are still there; "New project" needs a pay-per-token planner key and
is not part of the product direction.

All of it was clicked through against a throwaway daemon on the host
(loopback, single-user, its own data directory and the scratch cluster),
except promote, whose API was already proven on `intake`. To do that again:
leave `data.public_host` out of the throwaway config, or the app is pointed at
the production cluster on 5432 and fails to log in.

Account has **Your agent**: pick Claude, Codex or Grok, paste the token, see
whether runs would work; Services nudges there when none is connected. A
change that conflicts with what went live meanwhile says so, lists the files,
and has one button that hands them to its own agent. Neither the conflict path
nor several changes at once has been run on the host yet; the tests cover
them. A failed install releases its machine and with it the service log that
says why, so the reason is only what the record says.

## The next milestone

The first of the known-wrong items, TLS at the origin, deferred by the owner.
Until it exists signing in to the panel sends its cookie over a plain-HTTP hop,
and the panel is where a person rather than a curl command drives the loop
above. After that, the isolation below, which is what stands between this and
a second account.

## What still blocks a second account

The agent credential is solved; isolation between machines is not.

- **The Firecracker driver boots guests but is not in use.** Deployed, but dbz-alpha still runs `local`: `Create`, `Exec`, `ExecStream`, `Destroy`
  and `OpenSession` work on dbz-alpha against real guests, about 2.2 s from
  `Create` to the first command. The disk is a ZFS clone of the golden
  snapshot, never a copy (the hard constraint: a 40 GiB copy per fork on these
  spinning disks is minutes); the jailer gives each guest its own uid and
  chroot; commands go over vsock to `cmd/dabberz-guest`. dabberzd still runs
  `local`, which runs every instance on the host as one shared account. What
  still blocks a second account: **guests can reach each other and the host's
  listening ports** (measured), there is no restart reconciliation, and the
  deployment ships none of the pieces. [FIRECRACKER.md](FIRECRACKER.md) says
  exactly what works, what was measured and what is left.
- **The agent's credential was process-wide; it is now per account, and the
  customer's own subscription (deployed).** Each account stores one
  credential, tagged with its provider -- `claude`, `codex` or `grok` -- with
  `PUT /v1/account/agent` (`{"provider": "claude", "credential": "..."}`; for
  Claude, the token `claude setup-token` prints). `GET` says which provider is
  set, whether it can run, and what runs would use; `DELETE` removes it. It is
  sealed in its own table (`user_credentials`, bound to the account and the
  provider, so a row moved or relabelled will not open) rather than as a repo
  secret, which every service would see. `internal/agentauth` resolves it;
  `agent.Runner` requires the run's account, refuses any provider but Claude
  with "not supported yet", and sets the token itself, over any repo secret of
  the same name. Codex and Grok are recognised and storable but have no runner:
  adding one is a runner, not a schema change. An account with nothing stored
  has its runs **refused** with a message saying how to add one -- never run
  on someone else's. No response, log line or audit entry carries the value.
  dbz-alpha runs with `claude.owner_fallback: true` (set at deploy, backup
  beside the file) so `intake`'s agent kept working; once the owner stores
  their own token under Account, turn it off. The rule in general: either store a Claude
  token for the owner's account with `PUT /v1/account/agent`, or set
  `claude.owner_fallback: true` in `/etc/dabberz/dabberz.yaml`, which lets the
  *owner's* runs (and only the owner's) use `CLAUDE_CODE_OAUTH_TOKEN` until
  they store their own. It is off by default; on with the variable empty, the
  daemon refuses to start. Without one of the two, every agent run on dbz-alpha
  is refused. Still platform-paid: the planning model's
  `DABBERZ_ORCHESTRATOR_API_KEY`, which contradicts the never-a-platform-key
  rule and needs the same treatment.

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
- The install tests left ~50 `forms serve` behind per run. The local driver's
  `Destroy` only removes the directory, and nothing stopped the services first.
  `newInstaller` now releases every workspace its store holds when the test
  ends, pass or fail, then fails the test and kills anything still running
  from the test's own directory (`internal/install/leaks_test.go`). Seen to
  fail, naming the stragglers, with the release removed.
