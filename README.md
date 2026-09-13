# devex

dabberz — an AI automation platform for developing code-based projects
automatically.

dabberz takes a request against a repository, splits it into independent
workstreams, and runs each one as a coding agent in its own microVM with its own
branch and its own live preview URL. Finished work is verified by driving a real
browser against that preview, then reviewed and merged. The loop is automatic;
it asks you only when a fix would contradict your instructions, when the
situation is genuinely ambiguous, or when an agent is stuck.

- [Requirements](docs/REQUIREMENTS.md) — what it is meant to do.
- [Architecture](docs/ARCHITECTURE.md) — how it is built, and what is not built yet.

## Status

The control plane is implemented and tested end to end against a local VM
driver: planning and the approval checkpoint, fork scheduling, preview
allocation and Caddy routing, repo-scoped secrets, the per-repo memory store,
the verify/fix loop, tripwires, and the merge gate.

The web UI is served by the daemon at its listen address. It opens on a
cross-repo overview — what needs you, what is in flight everywhere, and how
much budget each workstream has left — backed by a single audit trail that
records every action across every repo, attributed to the user or to the
component that took it. Secret values never enter that trail.

A task runs end to end on the local driver: plan, approval checkpoint, fork
scheduling with fork-time serialization, preview allocation, coding, browser
verification, the merge gate, and completion — with spend accounted against
each workstream's tripwire budget.

Booting real Firecracker guests is **not** implemented yet — the driver's
networking, layout and machine configuration are, but the boot path reports
`ErrNotSupported`. See [Architecture](docs/ARCHITECTURE.md#what-is-not-implemented)
for the full list.

## Quick start

```sh
make check                      # fmt, vet, race tests
make build                      # bin/dabberzd, bin/dabberzctl

cp configs/dabberz.example.yaml configs/dabberz.yaml
export DABBERZ_MASTER_KEY=$(./bin/dabberzctl keygen | head -1)
make run
```

Then open the printed address in a browser, or drive it from another terminal:

```sh
./bin/dabberzctl status
./bin/dabberzctl repos add myapp git@github.com:you/myapp.git
./bin/dabberzctl tasks new <repo-id> "add ratings, photo upload and rank notes"
./bin/dabberzctl tasks approve <task-id>
./bin/dabberzctl events follow <task-id>
```

`dabberzctl help` lists the rest.
