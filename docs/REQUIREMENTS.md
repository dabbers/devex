# dabberz — Project Requirements

dabberz generalizes the existing dab.im "vibe coding" pattern (Dokploy + Caddy + Claude Code) into a platform for automated, largely hands-free LLM-driven development across an unbounded set of projects, with parallel independent workstreams per repo.

**Positioning:** dabberz previously served as a webserver/game server host. v1 here is being built for personal use, but the long-term intent is to carry that hosting identity forward as an agent-development service with live web previews — hosting other people's in-flight development the way it once hosted their running servers.

## Goals

- Agent has full access *within its sandboxed environment*; a separate, narrower permission set governs anything touching the orchestration layer itself.
- Unbounded project list implies a queue/scheduler — not all projects are "active" at once.
- Instance concurrency is bounded by machine resources; needs per-instance resource quotas and an eviction/queueing policy (open item, see below).
- Driveable via web (v1), Discord/Telegram (later).
- Forks are **independent parallel workstreams** (e.g. "add ratings," "add photo upload," "add rank notes" on the same repo), not competing attempts at the same task.

## Agent Roles

1. **Coding agent** — Claude Code, using the existing `CLAUDE_CODE_OAUTH_TOKEN` setup, with a dabberz MCP toolset attached (secrets, preview control, browser driving). Fixed to Claude Code for v1 — kept as the single execution layer rather than integrating a separate framework, to avoid passing state back and forth between systems.
2. **Orchestrator** — external, outside Claude Code's reasoning loop. Model pluggable via an OpenAI-shaped API (DeepSeek preferred initially). Owns:
   - Decomposing a user request into discrete, forkable workstreams, with a planning checkpoint where it can ask the user clarifying questions before spinning up any VMs.
   - Fork/spawn/kill decisions.
   - Overlap detection **at fork time** — a semantic ("vibe") judgment on whether two tasks are likely to collide (e.g., both touch image handling) and deciding to serialize or parallelize accordingly. This is decided upfront; there's no live sibling-awareness tool or mid-flight warning system in v1.
   - Tracking active agents **per repo scope** (not global across all repos).
   - Enforcing escalation tripwires (below).
   - Routing merge targets per user-configured preference.
3. **Verifier** — checks correctness and code quality, and drives a **real, non-headless** browser against the live preview to validate actual behavior (not just a DOM/diff check).
4. **Merge/review agent** — handles conflict resolution and a code-quality gate before landing a finished fork.

## Isolation Model

- Firecracker-style microVMs, not containers — needed for "full access to itself" and for agents to drive a real browser (computer use), not just headless tooling.
- Snapshot/restore from a golden image is recommended to keep fork spin-up fast despite full-VM isolation (not yet decided — flagged below).
- Full dev toolchain support: Go, Rust, Node.js, etc.
- Self-hosted databases available per project: Redis, Postgres, etc.
- Environment is intentionally ephemeral and can get messy — that's acceptable, it's dev-only.
- Each fork gets its **own VM**, entirely separate — no shared mutable state between forks, so agents can't step on each other's running processes or file changes.
- Production/staging remains a separate path: finished work is built out to the existing Dokploy-managed Docker Compose stacks. dabberz itself is scoped to in-flight dev work only.

## Secrets & Config Hierarchy

- Repo is the root of the hierarchy.
- Secrets and shared architecture/config live at the repo level and are inherited by every project/fork under that repo.
- Centralized secrets store, scoped per repo.

## Verification / Browser Driving

- A **shared UI VM** serves all verifiers rather than one VM per verifier.
- Multi-profile / extension-controlled browser model (similar to the Claude-in-Chrome pattern): one browser, N independently-addressable profiles/tabs — no need for N separate browser processes or VMs.
- Concurrency capped by the shared UI VM's capacity. When full, new verification jobs **queue and wait** (v1 — no auto-scaling to a second UI VM yet).
- Verifier hits the live preview URL over the network, through the same reverse proxy an external user would use — not a co-located shortcut.

## Verify → Fix Loop

- Fully automatic by default: verifier failures feed back to the coding agent to fix and re-verify with no human involvement.
- Escalates to the user only when:
  1. The fix conflicts with given instructions.
  2. The situation is genuinely ambiguous — no clear direction.
  3. The agent is stuck/broken beyond its own ability to resolve — enforced via a tripwire.
- Tripwire = combination of max fix/verify cycle count, max token/cost spend, and max wall-clock time. Thresholds are **global**, not per-task, for v1.

## Task Decomposition & Forking

- Orchestrator receives the full user request and splits it into discrete, independently parallelizable workstreams, with a back-and-forth planning checkpoint with the user before any VMs spin up.
- Overlap is judged semantically at fork time (see Orchestrator role above) rather than via a live sibling-awareness system.

## Merge Process

- Each finished, verified fork triggers a merge/review pass.
- **Merge target** (repo's main branch vs. a unified project/feature branch) is a per-task user preference — not assumed.
- **Merge timing** (land immediately on completion vs. wait for the full batch of forks in a task) is also user-configurable, not assumed.
- The merge/review agent handles conflict resolution and a quality gate before landing.

## Preview Environments

- Builds directly on the existing dab.im pattern: Caddy reverse proxy with wildcard TLS, MCP preview server, `preview.dab.im`-style URLs.
- Needs generalizing to dynamic port/URL allocation per fork (e.g. `preview-{project}-{fork}.dab.im`) with Caddy config regenerated per new instance.
- No deploys, refreshes, or cache clears required — the user and the verifier both hit the same live, in-flight preview.

## Environment / Infra

- Runs on a single large machine (v1), capable of running many Firecracker microVMs with port forwarding.
- A single reverse proxy (Caddy, reusing the existing config pattern) routes requests to the correct project/fork instance.

## Monorepo Support

- A single repo can contain multiple distinct projects; needs a discovery mechanism (open item, see below).

## Driving Interfaces

- v1: web only. The web control plane includes a workspace area for the selected project/fork with:
  - A web shell and SSH access into that fork's VM, so the user can inspect and manage the development environment directly.
  - An embedded VS Code view for editing and browsing workspace files inline.
  - An inline browser for opening the fork's live preview and navigating development URLs without leaving the workspace.
- Later: Discord and Telegram as additional chat-driven interfaces to the same orchestrator.

## Monorepo Discovery (resolved)

- No manifest file required. An LLM pass inspects the repo structure and infers the projects present, then confirms/clarifies with the user once per repo.
- dabberz keeps an out-of-repo notes/memory store per repo, recording what it's learned about the repo's structure and the user's preferences over time, so later passes don't re-derive everything from scratch. (The design of this memory store is itself a follow-on item — not detailed yet.)

## Resource Quotas & Eviction (resolved)

- No pre-emption of running forks. When the machine is near capacity, new fork requests **queue and wait** — same policy as the verifier queue, one consistent behavior across the system.

## Auth / Access Model (resolved)

- Single-user for v1 (you only). Data model (projects/tasks/repos) should still be scoped by a user id from the start, so multi-user support later is additive rather than a rewrite.

## Claude Code Usage / Rate Limits (resolved)

- No artificial concurrency cap on coding-agent VMs. Let usage run until a rate limit is actually hit, then surface/handle it as an error — likely folded into the existing tripwire/pause-and-retry pattern rather than a separate mechanism.

## Golden Snapshot Strategy (resolved)

- A single generic golden image, provisioned with the full toolchain up front (Go, Rust, Node.js, common DBs, browser stack) — not per-toolchain images. Prioritizes simple initial setup over minimizing per-VM boot size/storage.

## Escalation Scope (resolved)

- Escalation is per-fork. When one fork in a task batch hits a tripwire and escalates to the user, sibling forks in that batch keep running independently — the whole batch doesn't pause.

## Preview URL / VM Lifecycle (resolved)

- No automatic teardown. A workstream's VM and preview URL persist indefinitely after merge or abandonment, so it can be reused or revisited later.
- Cleanup is entirely manual, on the user — the system will not reclaim disk/capacity on its own, even if this eventually contributes to hitting machine resource limits.

## Memory Store Design (resolved)

- Scope is broad: anything applicable to a repo that agents need to know but can't easily grep from the codebase — layout, coding standards, design preferences, design language, UI element patterns, color schemes, architectural decisions, and stated user preferences.
- Structure: a **structured markdown folder per repo**, with an index file plus a folder of individual finding files (one topic/finding per file), so an agent can pull just what's relevant to its task rather than loading everything.
- Writes: each fork's coding agent writes **directly** to the store as it learns things — no orchestrator serialization. Last-write-wins on conflicts, consistent with the project's overall tolerance for a messy, dev-only environment.
- Updated continuously as understanding of the repo evolves, not just seeded once at monorepo-discovery time.

## Remaining Open Items

- Discord/Telegram interface design (explicitly deferred, not v1).
- Design of the out-of-repo notes/memory store (what it tracks, how it's structured, how it's updated over time).