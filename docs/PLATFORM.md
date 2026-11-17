# The platform contract

What an app must declare and do to run on dabberz, and what dabberz guarantees
in return.

This is written before the second app rather than after the fifth, because the
contract is the product. A catalog where each app is bespoke is a content
treadmill: every addition costs a full product's worth of build, patch and
support, and the margin goes with it. A catalog where every app sits on the
same substrate is additive — app six costs what its own domain logic costs and
nothing else. The difference between those two businesses is entirely whether
this document exists and is enforced.

It is also the document the coding agent is pointed at. A customer's agent
works inside these rules; the rules are what keep a sentence typed into a chat
box from taking down a booking page that takes real money.

## Two words

An **app** is a product in the catalog: Forms, Links, Helpdesk. dabberz builds
it, owns it, and publishes it as open source. It has releases.

An **installation** is one customer's copy of one app: its own domain, its own
database, its own users, its own changes. It pins an app version and may carry
changes of its own.

The distinction matters everywhere below. "Instance" is deliberately not used
for either: `vm.Instance` already means a machine.

## The manifest

Every app declares itself in `dabberz.yaml` at its repository root. The
platform reads it and nothing else: there is no convention-over-configuration,
because a convention is a thing an agent can break without noticing.

```yaml
# Identity.
app: forms
name: Forms
version: 1.4.0
platform: 1              # the contract version this app targets
license: AGPL-3.0        # what dabberz publishes it under
upstream: https://github.com/dabbers/app-forms

# The processes this app runs. Reuses the Service model the control plane
# already has: dependency order, health, which one is the front door.
services:
  - name: web
    kind: web
    command: node server.js
    port: 3000
    health_path: /healthz
    primary: true
    exposed: true
  - name: worker
    kind: worker
    command: node worker.js
    depends_on: [web]

# What the platform must provision before the app starts.
requires:
  database:
    engine: postgres
    version: "16"
  identity: true         # the app does not implement login
  mail: true
  storage: true
  payments: optional     # Stripe Connect, on the customer's own account

# Secrets this app needs, by name only. Declared so the platform can ask the
# customer for them at install time rather than letting the app fail at boot
# with a stack trace nobody can read.
secrets:
  - name: TURNSTILE_SECRET
    description: Spam protection for public form submissions.
    required: false

# Agent access. On by default, in every app, at no charge, in every plan.
mcp:
  path: /mcp             # served by the primary service
  tools: tools/          # definitions, versioned with the app

# Scheduled work. The platform invokes it; the app does not run its own cron.
jobs:
  - name: digest
    schedule: "0 13 * * 1"
    path: /jobs/digest

# Where a customer's agent is allowed to work. Everything outside this is core.
extends:
  theme: theme/
  plugins: plugins/
  config: config/
  schema_prefix: ext_

# The critical paths. A change that breaks one of these does not reach
# production. Run from the pinned upstream version, never from the
# installation's working tree -- see "Checks the agent cannot edit".
checks:
  suite: checks/
```

### Why these fields and not others

**`services` reuses what exists.** `domain.Service` already carries kind,
command, port, health path, exposed, primary and `depends_on`, with
topological start ordering and cycle detection. The contract does not invent a
second way to say the same thing.

**`requires` is a request for provisioning, not configuration.** The app says
it needs Postgres; it does not say where. The platform provisions, injects the
connection string, and owns the backups. An app that reaches for its own
database or its own mail transport has left the contract and cannot be
operated.

**`secrets` names without valuing.** Names only, ever: the vault seals values
and nothing else in the system — audit trail, UI, agent prompt, upstream pull
request — may carry one.

**`extends` is the whole design.** See below.

## What the platform provides

Each of these is the platform's to run, back up and keep working. An app that
uses them inherits that; an app that routes around them is on its own and
should be refused.

| Service | Platform owns | App owns | Injected as |
|---|---|---|---|
| **Database** | Provisioning, connection pooling, backups, point-in-time restore, snapshots | Schema, queries, migrations | `DATABASE_URL` |
| **Identity** | Sign-in, sessions, the user directory, federation out to Google/Microsoft/SAML | Authorization — who may do what inside the app | `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` |
| **Secrets** | Sealing, injection, rotation | Reading them from the environment | named env vars |
| **Mail** | Sending domain, DKIM/SPF, reputation, bounces, suppression | Composing the message | `SMTP_URL` |
| **Storage** | Buckets, per-installation prefixes, scoped credentials, lifecycle | What it puts there | `STORAGE_*` |
| **Jobs** | The scheduler, retries, overlap prevention | The handler at the declared path | invoked over HTTP |
| **Payments** | Brokering Stripe Connect onto the customer's own account | Prices, checkout, webhooks | `STRIPE_ACCOUNT_ID` and scoped keys |
| **Domains** | Custom hostnames, TLS issue and renewal | Nothing | the app sees a normal request |
| **Agent access** | Publishing the MCP endpoint, authenticating it, auditing every call, fair-use limits | Defining the tools and honouring its own permissions | served at `mcp.path` |
| **Observability** | Logs, health checks, the audit trail | Reporting health honestly | `/healthz` |

Three of these deserve their reasoning spelled out, because they are the ones
an app author will be tempted to do themselves.

**Postgres and only Postgres.** One engine means one backup story, one restore
procedure, one snapshot mechanism, one pooler and one migration runner. A
second engine doubles the operational surface and buys a customer nothing they
can perceive. The control plane itself runs on SQLite; apps do not get that
choice, because the control plane is one process on one machine and an
installation is somebody's live business.

**The app never implements login.** This is the highest-leverage rule in the
contract and the least negotiable. It is what makes five apps feel like one
product instead of five passwords, it removes the most frequently botched
security surface from every app in the catalog, and it is what makes an
account-wide user directory — and selling federation to an enterprise IdP —
possible at all. The app speaks OIDC to the platform and trusts what it gets.

Basic single sign-on across a customer's own apps is included and always will
be. Charging for it would be the exact behaviour this product exists to escape.
Connecting a customer's *own customers* through their Okta or a SAML IdP is a
different thing with real support cost, and may be charged for.

**Mail is centralised because deliverability is a specialty.** Every app here
sends: confirmations, notifications, password resets, digests. Reputation,
bounce handling and suppression are miserable, shared, and exactly the kind of
work a customer fleeing SaaS does not want to discover they have inherited.

## What the app must provide

In exchange:

- **A manifest that is true.** If it declares a health path, that path must
  report real readiness. A service that answers `200` before it can serve is
  worse than one that answers nothing, because the platform will route to it.
- **Forward-only migrations**, runnable non-interactively, idempotent on
  re-run.
- **No state outside the declared services.** Nothing written to local disk
  that must survive; nothing cached in a process that another process needs.
- **Honest failure.** Exit non-zero, log the reason, do not retry forever in
  silence.
- **A check suite** covering the paths whose breakage costs the customer money
  or trust: for Forms, submit → store → notify. Not coverage; consequences.
- **An MCP endpoint.** Its own operations, exposed as tools. Not optional, and
  not a paid tier — see below.
- **No credentials of its own.** Everything it needs arrives in the
  environment from the platform.

## The extension surface

The core/extension split is what makes agentic modification survivable. It is
the answer to the question that otherwise kills this model: *if every customer's
agent edits the app, how does anyone ever ship an upgrade again?*

**Core** is everything outside `extends`. It is dabberz's, it is the same bytes
in every installation, and it can be upgraded in place.

**The extension surface** is the four paths named in `extends`, and an
installation may contain anything there:

- **`theme/`** — tokens, templates, partials. "Make it match my brand" lands
  here and nowhere else.
- **`config/`** — settings the app already understands.
- **`plugins/`** — modules registering against documented hooks. New
  behaviour: an extra notification channel, a field type, a webhook on
  submission.
- **`schema_prefix`** — a reserved table and column namespace. Extensions may
  create tables and add columns inside it. They may never alter a core table,
  because the next core migration assumes its own shape.

The hooks an app offers are part of its public interface and are versioned with
it. An app that offers too few hooks will be modified in core by an agent that
had no other option, which is the failure mode this whole structure exists to
prevent. Designing the hook surface *is* designing the app.

### Checks the agent cannot edit

A gate the agent can edit is theatre. So the check suite is not run from the
installation's working tree: it is run from the **pinned upstream version of
the app**. The installation's changes are the thing under test; the test itself
comes from outside the installation.

A customisation that legitimately changes a critical path therefore cannot
quietly rewrite the check to match. It needs either a pull request that changes
the check upstream, or an explicit human override recorded in the audit trail
with a reason. Both are visible. Neither is the default.

## From a sentence to production

The path a change takes, and which parts the platform *enforces* rather than
requests.

1. **A customer types a sentence.** The agent runs in a workspace on a machine
   the platform gave it, with the installation's repository checked out.
2. **The agent changes the extension surface.** Rules in the repository tell it
   where it may work and why.
3. **A preview appears.** Its own hostname, its own copy of the database,
   seeded from a snapshot rather than pointed at production.
4. **Checks run.** The app's suite, from upstream, plus the browser harness
   against the preview: it loads, it rendered something, no uncaught
   exceptions, nothing 404s.
5. **A human promotes.** Not the agent. Ever.
6. **The platform deploys**, snapshotting the database first if migrations are
   pending.

Four rules the platform enforces structurally, so that a confused or hostile
agent cannot work around them:

- **The agent holds no credential that reaches production.** Not a policy the
  agent is asked to respect — it simply does not have one. Promotion is
  performed by the control plane on a human's instruction.
- **The agent cannot see another installation.** Per-tenant isolation at the
  hypervisor, which also keeps one customer's attached Claude credential away
  from another's.
- **Secrets by reference.** The agent is told `STRIPE_SECRET_KEY` exists. It is
  never told what it is.
- **Preview data is a copy.** An agent cannot destroy production data by
  experimenting, because what it can reach is not production data.

## Upstream: one customer's bug fix is everyone's

The interesting case is a change that is not a customisation at all — the
customer hit a bug in core.

Forking for that is the worst outcome: it leaves the installation permanently
diverged from a line of code that upstream will keep changing, over a defect
that every other installation also has.

So a change is classified by **where it lands**, which is mechanical rather
than a judgement:

- **Lands entirely in the extension surface** → it is a customisation. It lives
  in the installation forever and upgrades rebase over it.
- **Touches core** → it is a product change. The platform opens a pull request
  against the app's upstream repository, and the installation **carries the
  patch** until that lands.

A carried patch is re-evaluated at every upgrade: if upstream now contains it,
it is dropped; if it still applies cleanly, it is kept; if it conflicts, the
upgrade stops and asks. An installation's distance from mainline is therefore
visible as a number — how many patches it carries — rather than something
discovered during an incident.

This makes the customer base a contributor base, keeps every installation near
mainline, and means a bug fixed once is fixed for everyone. It also has three
costs worth naming now:

- **Inbound review.** Agent-authored pull requests arrive and need a human.
  This is real work and it scales with customers.
- **Contributions must be ownable.** A DCO or CLA on the app repositories,
  from day one, or dabberz cannot relicense or defend its own code.
- **Customer context must not leak upstream.** A prompt may contain private
  business detail, a carried diff may contain a customer's own copy. An
  upstream pull request is generated **from the diff of core files only**,
  never from the conversation, and never including anything from `config/`,
  `theme/`, `plugins/` or the extension schema. This is a hard rule and wants a
  test, not a guideline.

## Every app is addressable by an agent

Every app exposes an MCP server. It is on by default, it is in every plan, and
it is never a line item.

Charging for agent access to a customer's own data is the SSO tax wearing a new
hat: take a capability that costs almost nothing to offer, withhold it, and
sell it back as an enterprise tier. This product exists to be the alternative
to that, and a pricing page with "AI access" on it would undo the whole
position in one line.

There are two layers of it, and they are different things:

- **Platform MCP** — operations. List my apps, open a change, show me the
  preview, run the checks, promote, roll back. This is the second front door
  onto the same tool layer the web chat drives, and it is what lets a customer
  point their own Claude at dabberz instead of using the chat box, or lets an
  on-call agent drive a fix.
- **App MCP** — data and actions. Read this week's submissions, create a
  response, close that ticket. Declared by the app, served by the app, mounted
  and guarded by the platform.

The second one is where the platform effect lives. One account with five apps
has one MCP endpoint across all of them, under one identity. "Look at this
week's form submissions, find the ones mentioning refunds, and open a ticket
for each" is a sentence that no individual SaaS can execute, because each of
them only sees its own corner. That is not a feature any incumbent can match
without giving up the per-seat model that pays for them.

### The rules that keep it from being a back door

**MCP is a view onto the same operations, under the same identity and the same
permissions.** It is not a second API with its own notion of who may do what.
If a user cannot delete a submission in the interface, the tool call fails for
the same reason, in the same code path. An app that implements its
authorization twice will get it wrong once, and the wrong one will be the one
nobody looks at.

**The platform authenticates; the app authorizes.** The MCP endpoint is
published by the platform at a stable per-installation URL and sits behind the
same OIDC identity as everything else. The app receives a request it can
attribute to a real user and decides what that user may do.

**Tools are public interface and version with the app.** Someone will build an
automation on them; renaming a tool breaks it. They belong in `tools/`, they
are reviewed like an API, and they change on the app's version boundaries.

**Destructive tools are declared and confirmed.** A tool that deletes, refunds
or sends to real people is marked as such, and the platform requires an
explicit confirmation step rather than trusting an agent's judgement in a loop.
The blast radius of "delete every submission" reached by an agent at three in
the morning is not the same as a person clicking it.

**Every call is audited with the identity that made it.** The audit trail
already exists and already distinguishes actor. This is both the trust story
and the only way "the agent did something strange" is ever diagnosable.

**Free is a pricing statement, not an engineering one.** An agent in a retry
loop generates load no human ever would, and that load is real money on real
machines. Fair-use rate limits apply and are published. The distinction worth
holding: a limit protects the service, a paywall extracts from the customer.
These are not the same thing and customers can tell.

### The side effect worth having

An app that must expose its operations as tools ends up with a documented,
authorization-aware operation layer, because there is nowhere to hide one that
isn't. The same discipline the health endpoint imposes on readiness, the MCP
surface imposes on the domain model.

And the tool definitions are the best description of an app that exists — which
means the customisation agent reads them too. An app that explains itself to
one agent has explained itself to the other.

## Data

Code and data roll back differently, and conflating them is how a "rollback"
button loses a day of bookings.

**Code rollback is cheap and always available.** Redeploy the previous build.
Nothing is lost.

**Data rollback is a restore**, to the snapshot taken before the promote, and
it **discards everything written since**. A customer restoring to before this
morning's change loses this morning's form submissions. The interface must say
this in those words before doing it.

So: migrations are forward-only; a promote with pending migrations snapshots
first, automatically; extension migrations are additive and confined to the
reserved namespace; and a change that alters the meaning of existing data gets
a louder confirmation than one that adds a column.

**Export is a feature, not a concession.** A customer can take their database
and their repository and leave. Saying so plainly is most of the trust this
product is asking for — the whole pitch is that they own it, and an owner who
cannot leave is a tenant.

## Versioning

The manifest declares `platform:`. The platform supports the current contract
version and the one before it. A change that would break a conforming app is a
major version, which means it comes with a migration path and a deprecation
window, not a flag day.

App versions are the app's own. An installation pins one and upgrades
deliberately.

## What this deliberately does not do

- **No arbitrary containers.** Apps are dabberz-built, so the runtime is known
  and the contract can be strict. A bring-your-own-Dockerfile tier is a
  different product with a different operational posture.
- **No third-party apps in the catalog.** A marketplace is a trust surface
  several times larger than this contract and should not be smuggled in by
  accident.
- **No multi-region.** One region until a customer is losing money over it.
- **No per-app special cases.** If an app needs something the contract does not
  offer, the contract grows. The first exception is the end of the contract
  meaning anything.

## The first app

**Forms**, and not because it is the easiest.

Pick the first app to exercise the *whole* contract, so the contract is proven
rather than asserted. Forms touches every platform service: a database for
submissions, identity for the admin side, mail for notifications, storage for
file uploads, jobs for digests, and optionally payments for paid submissions.
Its critical path is short and genuinely checkable — submit, store, notify —
and the customisation story is both obvious and visual, which is what makes the
agent's value legible to someone who is not a developer.

The thing being proven is not the form builder. It is the loop: they deploy it,
use it, ask for a change in English, the change goes through preview and
checks, they promote it, and it works.

## Scheduling

Deferred, and not on technical grounds.

A scheduling app cannot be built while the owner of this project works at
Calendly. Note that an MIT-licensed starter solves the *copyright* question and
not the *employment* one: forking a permissively licensed scheduling app and
operating it commercially is still shipping a scheduling product. That wants
clearing in writing with an employer before any code exists, not after.

It changes nothing about the plan in the meantime — scheduling was not going to
be the first app regardless. When it does come up, the licence shape wanted is
MIT or Apache-2.0, so the fork can be owned outright with no upstream
obligation. Cal.com, the obvious candidate, is AGPL-3.0 and therefore the wrong
shape for that purpose, though the right shape for apps dabberz writes itself.
