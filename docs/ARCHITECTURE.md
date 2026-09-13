# dabberz architecture

This describes what is built, how the pieces fit, and where the boundaries are.
It follows [REQUIREMENTS.md](REQUIREMENTS.md); where a decision is load-bearing
the reason is given, because the reason is usually the part that gets lost.

## Shape of the system

dabberz runs on one large machine. A control-plane daemon (`dabberzd`) owns a
SQLite database, a scheduler and a VM driver; each in-flight workstream gets its
own microVM; one Caddy instance fronts every preview; one shared UI VM drives
the browsers that verify them.

```
                        ┌──────────────────────────────────────────┐
   web UI / dabberzctl ─▶│  dabberzd                                │
                        │                                          │
                        │  api ── orchestrator ── scheduler        │
                        │           │                  │           │
                        │           │            pipeline (1/fork) │
                        │           │             │   │   │        │
                        │        store        agent verify merge   │
                        │      (SQLite)          │   │   │         │
                        └────────────────────────┼───┼───┼─────────┘
                                                 │   │   │
                        ┌────────────────────────▼───┼───▼─────────┐
                        │  vm.Driver (firecracker | local)         │
                        └───────┬──────────────┬─────────┬─────────┘
                                │              │         │
                         fork VM│       fork VM│  shared UI VM
                         (agent)│       (agent)│  (one browser,
                                │              │   N profiles)
                                ▼              ▼         │
                        ┌──────────────────────────────┐ │
                        │ Caddy: *.dab.im, wildcard TLS│◀┘
                        └──────────────────────────────┘
                                 preview-{project}-{fork}.dab.im
```

The verifier reaches a preview through Caddy, the same way an external user
would, rather than talking to the dev server directly. A preview that only works
from inside the machine is not a working preview.

## Packages

| Package | Responsibility |
| --- | --- |
| `internal/domain` | Entities and the task/fork state machines |
| `internal/store` | SQLite persistence, migrations, the event log |
| `internal/vm` | Isolation interface and capacity accounting |
| `internal/vm/firecracker` | Production driver: layout, networking, jailer, machine config |
| `internal/vm/local` | Development driver: host directories and processes |
| `internal/scheduler` | Fork admission, plus the FIFO gate the verifier queue uses |
| `internal/preview` | Per-fork hostname and port allocation |
| `internal/proxy`, `internal/proxy/caddy` | Publishing routes to the reverse proxy |
| `internal/secrets` | Repo-scoped secrets, encrypted at rest |
| `internal/memory` | The out-of-repo notes store, one markdown folder per repo |
| `internal/llm` | OpenAI-shaped client for the orchestrator model |
| `internal/tripwire` | Global cycle/token/cost/wall-clock thresholds |
| `internal/orchestrator` | Planning, overlap judgement, escalation, merge windows |
| `internal/agent` | Driving Claude Code inside a fork VM |
| `internal/verify` | Driving a real browser against a live preview |
| `internal/merge` | Quality gate and conflict resolution |
| `internal/pipeline` | One fork, admission to merge |
| `internal/api` | The control-plane HTTP API |
| `internal/web` | The embedded control-plane UI |
| `internal/config` | Configuration and startup validation |

## Lifecycles

A **task** is one user request. It cannot start work without passing the
planning checkpoint:

```
draft ──▶ planning ──▶ awaiting_plan ──▶ running ──▶ completed
               ▲            │                   └──▶ failed
               └────────────┘
        (user answers questions; a new round refines the plan)
```

A **fork** is one workstream, owning exactly one VM and one preview URL:

```
queued ──▶ provisioning ──▶ coding ──▶ verifying ──▶ awaiting_merge ──▶ merging ──▶ merged
                                          │  ▲                              │
                                          ▼  │                              │
                                        fixing◀─────────────────────────────┘
                                                   (review requested changes)

any working state ──▶ escalated ──▶ (resumes where it left off)
```

Two properties are enforced by tests rather than convention: every state is
reachable from `queued`, and every state can reach a terminal state, so no fork
can be stranded. There is deliberately **no edge from `coding` to
`awaiting_merge`** — nothing reaches the merge gate unverified, which is also
why the pipeline refuses to start without a verifier.

## Decisions worth keeping

**Firecracker, not containers.** Agents need full access to their own
environment and need to drive a real browser. A shared kernel gives neither
safely. Each fork boots a copy-on-write overlay of a single generic golden image
carrying the whole toolchain, so spin-up never waits on a per-toolchain build.

**The local driver exists for development only.** It provides the same
interface and the same capacity accounting, and no isolation whatsoever:
commands run as the control-plane user on the host. It is what lets the
orchestrator, scheduler, routing and verify/fix loop be developed and tested on
a machine without KVM, including CI.

**One policy for contention, applied twice.** Nothing is ever pre-empted. When
the machine is full, new forks queue and wait; when the UI VM's browser profiles
are all busy, verification jobs queue and wait. The FIFO gate behind the
verifier queue is deliberate: a buffered channel wakes a random waiter, so a job
could starve behind later arrivals.

**Overlap is judged once, at fork time.** The orchestrator decides
semantically which workstreams would collide and puts them in a serialization
group; the scheduler admits one fork per group at a time. There is no live
sibling-awareness system, so if this judgement is lost nothing downstream will
catch the collision. Groups are scoped per task, because group names come from
one task's plan and carry no meaning across tasks.

**Escalation is per-fork.** A stuck workstream pauses alone; its siblings keep
running. An agent reaches the user by emitting an explicit marker, so
escalation is a decision it makes rather than something inferred from prose.

**The tripwire is what ends a non-converging loop.** Cycles, tokens, cost and
wall-clock, global rather than per-task. Resuming from a tripwire clears the
cycle counter, or the fork would trip again immediately and strand itself.

**The merge gate is closed by default.** Anything that is not an explicit
approval blocks the merge. The target branch is merged into the fork, not the
other way round, so a bad merge never reaches a shared branch; a claimed
conflict resolution is checked against the git tree rather than trusted.

**One UI VM, provisioned once and recognised again.** The shared UI VM is
found by a role label rather than a remembered id, so a control-plane restart
reuses the machine it was already using instead of stranding it and booting a
second — which matters because nothing is reclaimed automatically. If it cannot
be provisioned the daemon still starts and serves the control plane, and says
that nothing can be verified, rather than refusing to start and leaving the
operator without the UI that would explain why.

**Nothing is reclaimed automatically.** A fork's VM and preview URL persist
after merge or abandonment, so work can be revisited. Cleanup is manual, and
port exhaustion says so rather than failing cryptically.

**Everything is scoped by user id.** v1 is single-user; the scoping is there so
multi-user support is additive rather than a migration of every table.

## The interface

The UI is set in Broadsheet, a newsprint system: near-black Source Serif 4 on
a paper ground, the two process inks used small and deliberately, and structure
carried by the serif scale and whitespace rather than by boxes or dividers. The
serif is the chrome; no sans-serif is introduced. Broadsheet shows no dark
surfaces, so the interface is light only.

It speaks a different vocabulary from the domain, deliberately and one-to-one:

| Interface | Domain | What it is |
| --- | --- | --- |
| repository | repo | the root of secrets, memory and config |
| project | task | one prompt, decomposed |
| sub-task | fork | one workstream, one machine, one preview URL |
| machine | VM | the microVM a sub-task runs on |
| validation | verification | a real browser driven against the preview |

One point where the design and the system disagreed is worth recording. The
design presented a project as a single machine working a queue of sub-tasks,
with one preview URL. The requirements give each fork its own VM, running in
parallel except where two would collide. The isolation model won: the project
screen lists sub-tasks that each carry their own machine, branch and preview,
and a serialized one says so. Keeping the design's framing would have made
fork-time overlap detection meaningless, since nothing would ever run at the
same time.

## Two things the interface needs from the VM layer

Running the agents needs only `Exec`: start a command, wait, read what it said.
The workspace needs two capabilities beyond that.

**A terminal, not a pipe.** `vm.Interactive` is an optional capability a driver
may implement, kept off `Driver` so that a driver which cannot open one is a
type assertion rather than a stub in every implementation. A session is a real
pseudo-terminal: programs that check for one -- editors, pagers, anything
drawing progress -- behave as they would over SSH. The browser reaches it over
a websocket, hand-written here because the whole surface needed is one endpoint
carrying keystrokes one way and bytes the other. Its origin check is load
bearing: the control plane has no authentication of its own, so without it any
page the operator visited could open a shell on their machine.

**Machines that outlive the control plane.** Nothing reclaims a VM
automatically, so a restarted control plane must find the machines it already
has. The Firecracker driver will rediscover running guests; the local driver
records each instance beside its workspace and reloads them, so a restart does
not orphan every fork's machine or boot a second UI VM. A directory with no
record is left alone rather than adopted.

## The audit trail

Every action lands in one append-only log, and the UI's unified activity view
is that log with filters applied rather than a separate summary that could
drift from it.

Each entry carries a **repo** and an **actor**. The repo is what lets one feed
cover every repository at once and still narrow to one. The actor is what
separates the two questions an audit trail exists to answer: what did I
authorise, and what did the agents do unattended. User actions taken through
the control plane — adding a repo, answering a planning question, approving a
plan, cancelling, resolving an escalation, setting or deleting a secret — are
attributed to the user; everything else is attributed to the component that
did it (orchestrator, scheduler, pipeline, agent, verifier, reviewer).

Two rules keep the trail safe to read. **Secret values never enter it**: a
mutation records the name only, and provisioning records which names were
injected into a fork's VM, because "which credentials did this agent get" is
the auditable question. And the UI treats every entry as hostile input — agent
output and verifier reports flow straight into the feed, so the page builds
nodes and sets text rather than assigning HTML, which a test enforces.

The feed is cursor-paged on the event sequence in both directions: forwards for
a live stream that resumes exactly where it dropped, backwards for reading into
the past.

## Two subtleties in the data layer

**Event ordering.** Identifiers sort chronologically only to millisecond
precision, so two events written in the same millisecond have no defined order.
The event log therefore carries an autoincrement sequence, and that — not the
id — is the stream cursor a reconnecting client resumes from.

**Timestamp format.** Timestamps are stored at fixed width. `RFC3339Nano` trims
trailing zeros, which breaks lexicographic ordering (`.1Z` sorts after
`.10001Z`), and SQLite compares these columns as text.

## What is not implemented

- **Booting Firecracker guests.** The driver's layout, networking, slot
  allocation, jailer invocation and machine-configuration generation are
  implemented and tested. `Create`, `Destroy` and `Exec` return
  `vm.ErrNotSupported`. Until that lands, the control plane runs on the local
  driver.
- **The embedded editor**, and SSH from outside the browser. The web shell is
  built: `vm.Interactive` opens a real pseudo-terminal inside a machine, and a
  sub-task's workspace carries a terminal beside its preview. An editor needs
  more than a terminal does, and dropping in over SSH proper needs a daemon
  in the golden image.
- **The verifier harness.** dabberz hands the UI VM a JSON job and reads a JSON
  report back; the browser-driving program itself is not in this repository.
- **The dabberz MCP toolset** given to coding agents (secrets, preview control,
  browser driving) — the configuration path is plumbed through, the server is not.
- **The golden image build.**
- **Discord and Telegram**, which the requirements defer past v1.

## Running it

```sh
make check                      # fmt, vet, race tests
cp configs/dabberz.example.yaml configs/dabberz.yaml
export DABBERZ_MASTER_KEY=$(go run ./cmd/dabberzctl keygen | head -1)
make run
```

The UI is served by the daemon at its listen address, from assets embedded in
the binary: there is no build step and no second process to deploy. The tradeoff
is that editing CSS or JS has no effect until the binary is rebuilt and the
daemon restarted — `make build` tracks the assets as prerequisites, but the
running process still holds the old copy.

Secrets stored under a master key cannot be recovered without it. The daemon
warns at startup about any configuration that is valid but leaves part of the
system inert.
