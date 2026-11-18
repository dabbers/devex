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

## Blocks, components, apps

Three layers, and keeping them straight is what keeps the catalog from being a
pile of unrelated products.

**Blocks** are runtime infrastructure: data, identity, secrets, mail, storage,
jobs, payments, domains, realtime, webhooks, search, import, agent access.
Shared across every installation of every app, operated once, amortised across
the whole customer base. The blocks are the company. The catalog is the
visible part, but app six is only cheap because the blocks already exist.

**Components** are UI and domain parts that recur across apps: a table view, a
kanban board, a record detail pane, a form renderer, an inbox, a calendar, a
rich text surface. Owned by dabberz, versioned, shared.

**Apps** are compositions of components over blocks, plus the domain logic that
is genuinely particular to them. An app should be thin. If an app is thick,
something in it belongs in a block or a component and has been written in the
wrong place.

This is why "just enough to be useful to eighty per cent of people" is
achievable rather than wishful: most of the eighty per cent is blocks and
components that already exist and already work. The app supplies the shape, not
the substance.

It is also why a **custom app builder** is not a separate product line. A
template is a saved composition; a custom app is a composition assembled on
request. Same machine, different starting point. Which means the component
library is the actual deliverable and the templates are its first customers.

Sequencing matters here and is easy to get wrong: **do not build the component
library first.** Build two or three real apps, see what genuinely repeats, and
extract. A library designed before its users is the classic framework trap —
it ends up full of abstractions nobody needed and missing the ones everybody
did.

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
| **Database credentials** | Minting one per start, never storing it | Reading `DATABASE_URL` | `DATABASE_URL` |
| **Mail** | Sending domain, DKIM/SPF, reputation, bounces, suppression | Composing the message | `SMTP_URL` |
| **Storage** | Buckets, per-installation prefixes, scoped credentials, lifecycle | What it puts there | `STORAGE_*` |
| **Jobs** | The scheduler, retries, overlap prevention | The handler at the declared path | invoked over HTTP |
| **Payments** | Brokering Stripe Connect onto the customer's own account | Prices, checkout, webhooks | `STRIPE_ACCOUNT_ID` and scoped keys |
| **Domains** | Custom hostnames, TLS issue and renewal | Nothing | the app sees a normal request |
| **Realtime** | The sync fabric: CRDT documents, presence, rooms, pub/sub | Deciding what is a document and what it means | `REALTIME_URL` |
| **Webhooks** | Signing, delivery, retries, dead-letter, replay — inbound and outbound | Emitting the events and handling the ones it receives | `WEBHOOKS_URL` |
| **Search** | Indexing, query, ranking | Declaring what is indexable | `SEARCH_URL` |
| **Import** | Source adapters, mapping, dry runs, resumable jobs | Describing its own schema as a target | invoked at install |
| **Export** | Packaging data and repository together, delivery, scheduling, retention | Rendering its own data in a documented, re-importable form | invoked on request |
| **Agent access** | Publishing the MCP endpoint, authenticating it, auditing every call, fair-use limits | Defining the tools and honouring its own permissions | served at `mcp.path` |
| **Observability** | Logs, health checks, the audit trail | Reporting health honestly | `/healthz` |

Three of these deserve their reasoning spelled out, because they are the ones
an app author will be tempted to do themselves.

**How a copy is made is the block's business.** A change works against a fork
of its installation's data, and the platform asks for one operation -- give me
a copy of this -- rather than orchestrating how. Today that is a dump and a
restore, which costs time and space proportional to the data. That is right at
this size and stops being right somewhere in the gigabytes, and the number that
decides is how long opening a change takes. When it is minutes rather than
seconds, three replacements are waiting behind the same method:

- **A copy-on-write filesystem.** ZFS or btrfs snapshot and clone is
  effectively free and pays only for what diverges, which is the property worth
  having. It needs a Postgres cluster per installation rather than one cluster
  holding many databases, since what gets cloned is a whole data directory --
  a real architectural change that brings per-tenant isolation and per-tenant
  point-in-time recovery with it.
- **A storage engine built for branching**, of the kind Neon popularised.
  Instant, and the customer's data leaves this machine, which sits badly with
  a product whose pitch is that they own it. Worth revisiting if self-hosting
  one ever becomes a weekend rather than a project.
- **Postgres's own `CREATE DATABASE ... TEMPLATE`**, a file copy rather than a
  logical replay. Faster, and disqualified: it requires no other session to be
  connected to the template, which means disconnecting the live installation
  in order to open a change against it.

**No connection string is ever written down.** Not in the vault, not in a row,
not in a log. The platform owns the role, so a workspace mints a fresh
credential when it starts and the old one stops working — which costs an
`ALTER ROLE` and means there is nowhere for a database password to be read out
of. The only thing holding a superseded credential is a process being replaced.

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

**Realtime is a block, not an app's problem.** A whiteboard or a collaborative
document that runs its own sync server is an always-on process per
installation, which is the cost shape that makes those categories look
unaffordable. One sync fabric, with rooms that spin up and down per document
and are shared across every app that needs them, changes the arithmetic
completely. The first app that needs it pays to build it; every app after
that gets it for the price of a connection string.

**Import is the door on the escape hatch.** Nobody leaves HubSpot if leaving
means retyping four thousand contacts, and nobody leaves Airtable if their
formulas do not come with them. Migration is the single largest adoption
blocker in every category in this catalog, it is the same work every time —
read a source, map it, dry run it, resume when it fails halfway — and it is
exactly the kind of thing that gets rewritten badly once per app if it is not
a block. It is also the most on-brand feature dabberz could ship: the product
is an exit, and an exit needs a door at both ends.

**Export is not import's poor relation.** A platform for rescuing people from
SaaS must not become the thing they were escaping, and a lock-in built from
"technically you can export, it just doesn't quite work" is the most common
kind. So export is a block with the same standing as import, it exists for
every app from that app's first release, and it is held to a property that can
actually be checked: **an app's export must be a valid import to that same
app.** Round trip, in the app's own check suite, every release. A promise about
data portability decays silently; a test does not.

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
- **An export that round-trips.** Every record the app holds, in a documented
  form its own importer accepts, tested as part of its check suite.
- **No credentials of its own.** Everything it needs arrives in the
  environment from the platform.

### The handshake, which is not a URL

Most of what the platform hands an app is a connection string. Two things are
not, and they were nearly left for each app to invent:

- **`DABBERZ_TRUSTED_PROXY=1`** is the platform asserting that this app is
  unreachable except through it. It is what makes the identity headers below
  safe to believe. An app that does not see it must refuse to serve anything
  behind an identity rather than assume the headers are real.
- **`X-Dabberz-User` and `X-Dabberz-Email`** carry the person the platform
  authenticated.

One set of names for every app, because an app that invents its own can be
started without it by accident and fall open. That is the failure that matters
most and it is silent: nothing is broken, the admin pages simply serve anyone
who asks.

### Where forwarded identity stops working

Forwarding is enough while an app answers on a hostname the platform owns and
can set a cookie for. It is not enough on a customer's own domain, and that is
not a detail — `forms.acme.com` is the whole point of bringing your own domain.

A browser on `forms.acme.com` carries no dabberz cookie and cannot be made to:
different registrable domain, different cookie jar. The platform's edge can
still route the request, but it has nothing to authenticate it with, so it
forwards no identity and the app correctly refuses its admin surface.

Which is where OIDC earns its place after all. Establishing a session on an
app's own hostname means a redirect out to the platform and back with
something the app's domain can hold — exactly the flow the protocol exists
for. So the position is narrower than "apps never speak OIDC":

- **On a platform hostname**, the edge authenticates and forwards headers.
  Simple, and enough for everything today.
- **On a customer's domain**, a session has to be established on that domain,
  and that is a redirect flow whether or not it is called OIDC.

Until that exists, an installation on a custom domain can serve its public
pages and not its admin surface. That is the safe failure rather than the
convenient one, and it is better than the alternative of trusting a header on
a hostname the platform does not control.

### Stripping is not optional

Every proxy in front of an app removes the identity headers from the inbound
request before forwarding. An app behind the platform is told to trust them,
so a visitor who could set one would hold the admin surface of somebody else's
installation — and nothing about it would look broken.

This is a property of the edge, not of the app, because an app cannot tell
which of its headers the platform added. It wants a test that forges one and
watches it fail to arrive.

### Nothing is injected that was not declared

An app that never asked for storage does not receive storage credentials, even
on a platform that has them. This keeps the blast radius of a compromised app
to the blocks it actually uses, and it makes `requires` mean something at
runtime rather than being documentation.

The inverse is enforced too: an installation whose manifest declares a block
the platform cannot provide **does not start**, and the refusal names every
unmet requirement at once. An app started without the mail it declared does not
fail at boot — it fails at three in the morning when the first notification
does not arrive, and by then nobody connects the two. Refusing is the kinder
failure.

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

### Checks, and the honest limit of them

The platform tells a suite two things and expects an exit code:
`DABBERZ_CHECK_URL` is the change's live preview, and
`DABBERZ_CHECK_DATABASE_URL` is a throwaway database it may empty. Never the
change's own copy -- a suite that truncates tables would leave the preview
blank and look like it had worked.

Two different questions get asked. The app's suite asks whether the paths that
cost a customer money still work, which only the app's author can know. The
browser harness asks whether the thing renders at all over its public address,
which no unit test notices and which is the most common way a change breaks
something.

A gate the change can edit is theatre, and the obvious fix -- run the suite
from the pinned upstream version instead of the change's tree -- does not
survive contact with how tests are actually written. For most toolchains the
test bodies compile with the code they test, so taking them from an older
revision either fails to build or tests the wrong thing. There is no version of
this that both tests the change and is immune to it.

So the honest answer is detection rather than prevention. The platform compares
the change against the revision the installation is serving and reports when it
has altered anything under the suite's path. That is not forbidden -- a check
sometimes has to change with the code it checks, and a platform that refused
would simply be routed around -- but it appears as a failed check, so a person
is told before they promote rather than afterwards. A change that rewrites its
own gate can still go live; it cannot do it quietly.

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

A change is one record, because it is one thing from the customer's side: the
branch, the preview, the checks and the promote are not four features. At most
one is open against an installation at a time — two means two previews, two
copies of the data, and a promote that has to reason about what the other one
did, which is a merge conflict presented to somebody who does not know what git
is.

Promoting is a fast-forward of the installation's branch onto the change's.
Anything else means something moved while the change was open, and resolving
that is a decision rather than a deploy.

A serving workspace is the one place a checkout is disposable. Everywhere else
in this platform the disk is somebody's work and nothing resets it; production
is the opposite, because nobody edits it by hand and "whatever is on the disk"
must not decide what customers see. A serving installation is the promoted
revision or it is wrong.

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

### Sharing an app's agent, without building entitlements

The owner of an installation can hand its agent surface to someone else —
a colleague, or a colleague's agent — scoped to that installation and nothing
else. "Dana can drive Forms with her own Claude. Dana cannot see anything else
in this account."

The thing that makes this shippable is refusing to build a permission system.
Granular entitlements are a matrix of roles against resources against verbs,
they are miserable to reason about, they are where enterprise SaaS hides its
upgrade pricing, and nobody running a ten-person company wants to configure
one. **The boundary is the installation.** One app, whole or not at all. That
is a sentence an owner can hold in their head, and it is the only scope this
product needs.

How it works:

- **A grant mints an identity; it does not bypass one.** The platform issues a
  credential bound to one installation, and when a call arrives it resolves to
  either a real platform user or a service identity created for that grant.
  Either way the app receives the same authenticated request it always does, so
  "a view onto the same operations under the same identity and permissions"
  stays true and **the app needs no code at all** to support this. Forms works
  with it unchanged.
- **Two modes, not a matrix.** Read-only, or full. Anything finer is the
  permission system this is refusing to build, and anything coarser cannot
  express "let them look but not send".
- **Off until the owner turns it on**, per installation. Sharing an agent is a
  decision, not a default.
- **Revocable in one click, and listed plainly**: who holds a grant on what,
  visible on the installation rather than buried in settings.
- **Every call attributed to the grant** in the audit trail, so "the agent did
  something strange" names which grant did it.
- **Rate limited per grant.** An agent in a loop is not a person, and a limit
  protects the service — which is not the same as a paywall.

Two boundaries that are not negotiable:

- **A grant never reaches the platform.** Operations MCP — open a change,
  promote, roll back, see other apps — is a separate credential belonging to
  the account owner. Someone who can read your form submissions must not be
  able to deploy to your site.
- **A grant is access to data, not to the code.** Changing the app is the
  owner's, through the platform.

And one thing to say out loud in the interface rather than discover later: a
grant is **bulk** access by its nature. Someone who can read submissions
through an agent can read all of them at once, which is different in practice
from the same person clicking through a list. The sharing dialog should say
that plainly instead of presenting a casual toggle.

### Why this is free

Because the alternative is the thing this product exists to replace.

The incumbent shape is: charge per seat, then charge again for API or AI access
on top, then gate sharing it behind a tier. Three separate extractions from one
capability that costs almost nothing to provide.

Here, seats are unlimited, agent access is included, and sharing agent access
is included. The feature an incumbent would make the premium SKU is the
default, and that is not generosity — it is the position. A pricing page with
"AI access" on it would undo everything else in this document.

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

**A rollback is one deep, deliberately.** A customer rolling back wants the
thing that was working ten minutes ago; a list of twenty revisions is a
decision they cannot make. The two revisions swap rather than the older one
being dropped, so rolling back is itself undoable by rolling back again.

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
- **No component library written ahead of its users.** Components are extracted
  from apps that already exist, never designed for apps that might. A block
  built before the shapes it must serve does not stay general: it accumulates
  a special case per app that arrived after it, until the abstraction is a
  pile of conditionals wearing a shared name.

## Where an installation's code lives

Git is load-bearing and GitHub must not be.

Every mechanism in this document is git underneath. An installation is a fork
of an app. A change is a commit, a preview is a branch, a promote is a deploy
of a known revision, a rollback is a redeploy of the previous one, and a
carried patch is a patch. None of that is negotiable.

But requiring a GitHub account to run a booking page is the exact friction this
product exists to remove. The buyer is a ten-person company who does not have a
developer, and "first, create an account on a code hosting site" is where they
close the tab. It also makes a competitor's uptime a dependency of ours.

So the platform hosts the repositories, and GitHub becomes optional. Three
postures, and the first is the default because most customers should never
learn that any of this is git:

- **Managed.** The repository lives on dabberz, created at install from the
  app's current release. The customer never sees it. Most installations stay
  here forever.
- **Mirrored.** The repository lives on dabberz and is pushed continuously to
  the customer's own GitHub. They hold a copy they control, and nothing on the
  critical path depends on it being reachable. This is the honest form of "you
  own this".
- **Origin elsewhere.** The repository lives in the customer's GitHub and
  dabberz works against it. For people who want their own review and their own
  CI. Per-account GitHub credentials already exist for this.

### What this actually needs built

Less than it sounds, because **the default posture needs bare repositories,
not a forge.** Issues, pull requests, code review, a web file browser — the
platform already has better surfaces for all of it. The change is a preview
with checks against it, and the review is a person looking at the running thing
and pressing promote. A smart-HTTP git endpoint serving bare repositories over
the platform's existing identity is the whole requirement.

A forge is needed in exactly one place, and it is dabberz's own: the
open-source app repositories that carried patches are sent upstream to. Those
can live on GitHub publicly, because that is dabberz's dependency rather than a
customer's, and because open source wants to be where contributors already are.

### What this changes about export

An export has to include the repository, not only the database. An installation
is code and data together — the data without the code is a table nobody can
run, which is a sophisticated way of being locked in. So the archive is the
database contents plus the git history, and a customer leaving can stand the
whole thing up somewhere else.

## What assumes one machine

The platform runs on one host today and will not always. Writing down where
that is baked in turns a future migration from a discovery into a list, and
costs nothing now.

Scaling this vertically is a dead end whatever the box: a single machine cannot
take a kernel update without downtime, its blast radius is everything at once,
and dedicated hardware does not live-migrate. So the direction is more hosts,
not bigger ones, and the useful thing is knowing which four places would have
to change.

- **The preview router** (`internal/proxy/hostmux`) resolves a preview hostname
  to a service in this process. Across hosts it has to resolve to a host first,
  which means the route table stops being in-memory and the edge stops being
  the same process as the control plane.
- **The data block** (`internal/database`) takes one admin connection string
  and provisions into one cluster. More hosts means either a cluster per host,
  with installations pinned to the host their data is on, or one cluster
  reached over a private network. The first is simpler and the second is what
  makes an installation movable.
- **The VM drivers** create instances on the machine they run on. Multi-host
  needs a driver that places work on a chosen host, which is mostly a matter
  of the scheduler already knowing capacity per driver rather than in total.
- **Snapshots** are written to a local directory. They have to be somewhere
  every host can reach before an installation can be restored onto a different
  machine than it was taken from — which is also the only way to prove a backup
  actually works.

None of these is hostile to more than one host; they just have not been asked
to do it. The scheduler already accounts for capacity, which is the part that
would have been hard.

The honest sequencing is to run on one cheap machine until something real is
constrained by it, and to add the second host before the first customer whose
downtime costs them money — because the second host is what buys a maintenance
window, not what buys capacity.

## Choosing what goes in the catalog

The test is not what is cheap to host. Cost is a property of the blocks, not of
the catalog, and letting it pick the apps would aim this product at categories
that are already commoditised — nobody is in pain over the price of a form
builder, because free ones are good.

The test is **what the incumbent's price is actually paying for.**

Expensive SaaS is rarely expensive because of hosting. It is expensive because
of the no-code configuration layer built on top: formula editors, workflow
canvases, field builders, view designers, automation rules, permission
matrices. That surface is most of their engineering, most of their tier
gating, and most of what a seat costs. It is also precisely the layer an agent
replaces — describing an automation in a sentence instead of dragging nodes
onto a canvas. Skip the configuration UI and what remains is a data model and
a runtime, which is the cheap half.

So, in order:

1. **Is the price justified by a configuration layer, or by a crafted core
   interaction?** An agent can replace a config UI. It cannot replace a text
   editor or a drawing canvas. A grid, a list, a mailbox, a form, a page — all
   solved interactions where a good implementation is genuinely good. A block
   editor or a canvas is the product itself, and eighty per cent of one is a
   tax paid every day rather than a saving.
2. **How often is the core interaction touched?** This is the sharper version
   of the question above, and it cuts within a product rather than between
   products. A wiki that is read daily and edited weekly tolerates an
   eighty-per-cent editor. A team that writes all day does not. A whiteboard
   used once a month for a retrospective tolerates a great deal. Replace the
   *use*, not the logo.
3. **Does every company configure it differently?** Variance is where the agent
   earns its place. Pipelines, routing rules and automations differ per
   company, which is why configuring them is a consulting industry. If
   everyone uses it the same way, the incumbent's defaults are fine and there
   is nothing to sell.
4. **What does it cost ten people?** The pain is sharpest between five and
   fifteen seats: enterprise-shaped pricing, no enterprise budget, and maybe
   five per cent of the feature surface in use.
5. **What happens when it breaks?** Data gravity is what makes these categories
   sticky and expensive to leave, and it cuts both ways. Losing a company's
   CRM is business-ending for them. Blast radius is not a reason to refuse an
   app, but it is a reason to not ship it before restores are routine.

Cost has not disappeared, it has moved. It no longer decides which apps exist;
it decides **which capabilities are blocks and which are per-installation**,
and it decides pricing. An app whose cost to serve is fifty times another's
cannot sit behind the same flat price without either overcharging the cheap one
or losing money on the expensive one.

## Pricing: one principle, and a deferral

The mechanism is deliberately undecided — it wants usage data that does not
exist yet, and designing a price for workloads nobody has run is how a platform
ends up defending a number instead of changing it.

One principle is decided, because it is the whole position: **never charge for
a person.**

The objection to per-seat pricing was never that larger customers pay more.
Almost everyone accepts that scale costs more. It is that per-seat pricing
*taxes collaboration* — adding a colleague so they can see one dashboard costs
a full seat, so people share logins or leave out someone who should be
involved, and the pricing makes the tool worse at its job. Charge on anything
that is not a human being.

Which rules out the obvious compromise. A "resource slice" sounds like cost
recovery, but resource consumption tracks headcount, so slices are seats with
extra steps — and worse, unpredictable ones. Honest about costs, hostile in
exactly the same way.

Two cautions worth carrying into whatever is eventually chosen:

- **Per-app pricing punishes composition.** If a CRM and a helpdesk are
  separate line items, customers will ask the agent to bolt helpdesk features
  onto the CRM to avoid the second bill. That is worse for them and it breaks
  the suite, which is the thing no incumbent can copy.
- **A limit is not a paywall.** Throughput allowances protect the service and
  scale with a customer's own success rather than their headcount. Gating a
  capability behind a tier is the move this product exists to be the
  alternative to, and doing it once makes the positions on single sign-on and
  agent access read as marketing rather than principle. That applies to
  realtime too, whatever it turns out to cost: included, with an allowance,
  not withheld and sold back.

## The first app

**Forms** — as the shakedown, not the flagship.

"Cheaper forms" is a dead wedge and should not be marketed as one. The reason
to build it first is that it is small, low stakes, and exercises the entire
contract: database, identity, mail, storage, jobs, payments, webhooks, MCP. Its
critical path is short and genuinely checkable — submit, store, notify. Writing
one real app is how the missing manifest fields are discovered, and that
discovery should happen on a form rather than on somebody's CRM.

It is not throwaway. The CRM needs lead capture and the helpdesk needs ticket
submission, so Forms becomes the first component of the suite rather than a
product line of its own.

The thing being proven is not the form builder. It is the loop: they deploy it,
use it, ask for a change in English, the change goes through preview and
checks, they promote it, and it works.

Then, in rough order of how well they satisfy the criteria above: **marketing
site and landing pages** (cheapest to serve, strongest agentic story, and the
lowest-commitment first install, which makes it the funnel entrance as much as
a product), **a base** — the grid, views, formulas and automations an
Airtable costs two hundred to four hundred a month for ten people — then
**CRM** and **helpdesk**, which are the same components pointed at different
domains.

They are deliberately a suite rather than a set. A lead arrives on the
marketing site, lands in the CRM, and support goes to the helpdesk, under one
sign-in and reachable through one MCP endpoint. No incumbent can offer that
without abandoning the per-seat model that funds it.

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
