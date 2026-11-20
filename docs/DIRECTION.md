# Direction: from batch orchestrator to live workspace

This document records a mid-build reframing of what dabberz is, what that
changes, and what it does not. `REQUIREMENTS.md` is the original spec and is
left as written; `ARCHITECTURE.md` describes what exists today. This file
describes where the product is going and why, so the reasoning survives the
conversation it came out of.

## The reframing

The original spec describes a **batch orchestrator**: the user writes a
request, an LLM decomposes it into parallel workstreams, the user approves the
plan, N VMs run to completion, each verifies itself, and the results merge. The
user's attention is needed twice — at the planning checkpoint and at the merge
gate — and the work in between is meant to be unattended.

The product the user actually wants is closer to **Lovable or Replit Agent**:
you describe a change, see it running within seconds, look at it, say "no, the
spacing is wrong", and see that too. The loop is conversational and the
feedback is visual. Where those tools stop short is the part dabberz already
has: they run one agent against one app in one container, they do not verify
their own work in a browser, and they do not isolate anything at the kernel
level.

Both modes are wanted. The conversational one should be primary, because it is
the one used minute to minute; the batch one becomes a mode you enter from it
("take these four things and do them in parallel") rather than the only way in.

## What this changes

### 1. A workspace becomes a first-class entity

Today the longest-lived thing in the model is a `Task`, which is born from a
request and dies at the merge gate. A `Fork` is created by a plan and ends in
`merged` or `abandoned`. There is no entity meaning "the environment I come
back to".

A `Workspace` is that entity: a repo, a branch, a VM, a set of running
services, and a conversation. It outlives any individual request. A `Fork`
then becomes a special case — a workspace on a scratch branch, created by a
plan instead of by a human, with a lifecycle that ends at merge. The scheduler,
the serialization groups, the verify/fix loop and the merge gate all keep
working unchanged, because they operate on forks.

### 2. Previews become per-service, not per-fork

This is the concrete blocker for "apps/multiple services". Today:

- `domain.Project` carries a single `PreviewCommand` and a single
  `PreviewPort`.
- `domain.Fork` carries a single `ProjectID` and a single `PreviewURL`.
- `preview.Allocator.Allocate` takes one fork and one project and returns one
  hostname bound to one upstream port.

A repo may hold several projects, and a fork may target any one of them, but a
fork cannot run a web app *and* its API *and* a worker *and* a database at the
same time. Lovable papers over this by hardcoding one shape (React plus
Supabase). The honest fix is a `Service` model: each service has its own
command, port, health check, log stream and preview route, and a workspace runs
a set of them. The preview allocator already allocates per (fork, project); it
needs to allocate per (workspace, service) and the route table grows a
subdomain level or a path prefix.

Service definitions should come from discovery where they can be inferred
(compose files, Procfiles, package scripts) and be editable by hand, because
inference will be wrong often enough that an unfixable wrong answer is worse
than no answer.

### 3. Agent sessions become resumable

`internal/agent` runs `claude -p --output-format json` once per invocation and
exits. It parses `session_id` out of the result and throws it away — the field
exists on the internal `claudeResult` struct and is referenced nowhere else.
Every fix cycle therefore starts from an empty context and rebuilds its
understanding from the prompt.

Claude Code supports resuming: `--resume <session_id>`, or `--session-id <uuid>`
to choose the id up front rather than parsing it back out. `--fork-session`
branches a conversation. For live output, `--output-format stream-json
--verbose --include-partial-messages` emits incremental text events and a final
`result` message; `--input-format stream-json` keeps a single process alive
across turns.

Two consequences worth recording:

- **Sessions live in the machine's `~/.claude` store.** A VM must persist that
  directory (or `CLAUDE_CONFIG_DIR`) across restarts for resume to work. This
  is a golden-image and volume-layout requirement, not an application detail.
- **Resumed sessions report cumulative cost.** `total_cost_usd` on a resumed
  run is the whole conversation's total, not the turn's. The tripwire
  accounting in `domain.Usage` adds each run's figures, so switching to resume
  without subtracting the previous total would multiply the cost it thinks it
  has spent. `--max-budget-usd` is available as a second line of defence.

Steering a run in flight is not supported. The documented pattern is interrupt,
then send the next message: SIGINT ends a turn cleanly (SIGTERM exits 143 and
leaves the turn unfinished with no result recorded). Set
`CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` to continue an interrupted turn rather
than start fresh.

### 4. The client surface has to assume a phone

"Run these agent sessions anywhere" means the UI is the product, not a console
for it. The current UI is a server-rendered page plus an SSE stream, which is
the right foundation — there is no client-side framework to make work on a
phone. What is missing is the layout work and, more substantively, the fact
that a web shell and an embedded editor are not usable on a phone. The phone
view should be: conversation, preview, activity, approvals. Shell and editor
stay desktop-only and that is fine.

## Authentication: the subscription question

Can this run on a personal Claude Code subscription rather than API keys? For
personal, single-user use: yes. As a hosted product serving other people: no.

Claude Code's own legal and compliance documentation states:

> OAuth authentication is intended exclusively for purchasers of Claude Free,
> Pro, Max, Ultra, Team, and Enterprise subscription plans and is designed to
> support ordinary use of Claude Code and other native Anthropic applications.

> developers building products or services that interact with Claude's
> capabilities, including those using the Agent SDK, should use API key
> authentication through Claude Console or a supported cloud provider.

> Anthropic does not permit third-party developers to offer Claude.ai login
> into their own applications, or to route requests through Free, Pro, Max, or
> Ultra plan credentials on behalf of their users. Moreover, developers may not
> collect, store, or intermediate Claude.ai credentials or session tokens.

> Customers may not pay for, resell, or intermediate Claude usage on their end
> users' behalf.

The same documentation says an end user may sign in to the **unmodified**
binary with their own subscription even where a platform hosts it, and that
preinstalling Claude Code in hosted sandboxes or agent infrastructure requires
agreeing to Commercial Terms unless otherwise agreed. Advertised Pro, Max and
Ultra limits "assume ordinary, individual usage of Claude Code and the Agent
SDK"; no concurrency limit is published.

The practical reading:

- **Single-user dabberz on the owner's own subscription is within ordinary
  use.** `claude setup-token` produces a one-year OAuth token exported as
  `CLAUDE_CODE_OAUTH_TOKEN`. Running several agents in parallel against one
  subscription is not separately prohibited, but it consumes the same session
  and weekly windows, so the ceiling is practical rather than legal.
- **The moment anyone else's work runs through that subscription, it is the
  prohibited case.** That path needs API keys, per-tenant workspaces for spend
  isolation, and a look at whether marking up Claude usage needs a commercial
  agreement.
- Design the credential layer so the token is **per user**, supplied by that
  user, never held centrally. That keeps the single-user path legal today and
  the multi-tenant path possible later without a rewrite.

Two mechanics that matter for how the agent is invoked:

- `-p` defaults to Manual permission mode in recent versions, which **denies**
  calls that would need approval. Unattended runs need `--permission-prompts
  none`; runs that should surface approvals in the dabberz UI need
  `--permission-prompt-tool` or a `PermissionRequest` hook.
- `--bare` never reads OAuth credentials or the keychain, and is recommended
  for scripted use; without it, `-p` loads project hooks and `.mcp.json` even
  in untrusted folders. Since dabberz runs agents over repositories it did not
  write, that distinction is a security decision, not a convenience one.

Pin the Claude Code version in the golden image: many of these flags have
minimum versions.

## What survives unchanged

Most of the infrastructure. The reframing is about the loop around it, not the
machinery underneath:

- **The VM driver abstraction** (`internal/vm`) and the Firecracker
  implementation. A workspace needs a machine for the same reasons a fork does:
  the agent runs real builds, a real browser and real servers, and a kernel
  boundary is the only honest answer to running other people's code.
- **Preview routing** (`internal/proxy`, `internal/preview`) — wildcard DNS-01
  TLS and whole-config regeneration. The allocation key changes; the mechanism
  does not.
- **The browser verify loop** (`internal/verify`). This is the thing Lovable
  and Replit do not have: the agent's own claim that it works is checked
  against a real page in a real browser. It becomes more valuable in a
  conversational loop, not less.
- **The audit trail** (`internal/store/audit`, the unified activity surface).
  Every actor is attributed and every action is appended. Nothing about that
  changes when forks become workspaces.
- **Tripwires, secrets and the merge gate.** The tripwire accounting needs the
  cumulative-cost correction described above; otherwise it stands.
- **The web shell** (`internal/api/shell.go`). A persistent workspace is
  exactly where a shell earns its place.

## Order of work

1. ~~`Service` model: schema, discovery, per-service preview routes.~~ **Done.**
   `domain.Service`, migration 0003 plus the 0004 backfill, per-service preview
   allocation, discovery that proposes and repairs service sets, and a UI that
   lists every service and switches the preview between them. Per-service log
   streams and active health checking are not done: a service records a health
   path but nothing polls it yet.
2. ~~`Workspace` entity: long-lived, owns a VM and a service set, with `Fork`
   re-expressed on top of it.~~ **Done.** `domain.Workspace` with its own
   start/stop state machine, `internal/workspace` as the single path from "a
   repo on a branch" to "a machine with services running and previews
   answering", and a fork that now owns a workspace rather than a machine.
   Preview routes are keyed by workspace, and a workspace can be opened,
   started, stopped and archived by hand, with its own shell, previews and
   audit trail. What is not done is the conversational part: a workspace has
   no agent session attached to it yet, so it is somewhere to work rather than
   somewhere to ask for work.
3. ~~Resumable agent sessions, with the cost-accounting fix and the session
   store persisted.~~ **Done.** A fork carries a conversation across its runs,
   so a fix round continues rather than re-reading the workstream; the session
   turns the conversation's running total into that run's share, so resumed
   runs are not charged for every earlier turn; a lost conversation falls back
   to a fresh one instead of failing the fork; and `agent.session_dir` puts the
   store on persistent storage. Streaming output is not done: runs are still
   read as one result at the end, so the interface cannot show an agent
   working, only that it worked.
4. ~~Conversational entry: a workspace conversation that drives the agent
   directly.~~ **Done.** A workspace carries its own conversation and a
   message log; a message runs the agent on that workspace's machine against
   the preview already open, and the reply lands in the log. The agent is told
   it is working with someone present rather than running an unattended
   workstream, so it does the thing and stops. One turn at a time per
   workspace, claimed in the same transaction that records the message, and
   turns orphaned by a restart are failed at startup rather than leaving a
   workspace busy forever.

   The agent's answer now arrives as it is written: `vm.Streaming` is an
   optional driver capability, and a run asks for it only when the driver has
   it and something is watching. The plan-and-fork path is still a separate
   door rather than something reachable from a conversation.
5. ~~The verification harness: a real binary behind `verify.driver_command`.~~
   **Done.** See below.
6. Mobile layout for the four surfaces that make sense on a phone.

## The verify loop closes

`verify.driver_command` named `dabberz-verify` from the beginning and nothing
answered to that name, so the fix loop was wired to a command that did not
exist. `cmd/dabberz-verify` is that command: it reads a job on stdin, drives a
real Chromium at the preview over its public URL, and writes a report on
stdout. A failing verification exits zero, because the report is the verdict
and the loop needs to read the reason to feed it back.

There is no browser-driver dependency. DevTools is JSON over a websocket, this
tree already had a hand-written RFC 6455 server for the terminal socket, and
the only thing the protocol asks a client to do differently is mask its frames.
So the framing moved to `internal/wsframe` and both ends use it: the control
plane as a server, the harness as a client. `internal/browser` adds the client
handshake, the command/event multiplexing, and a page visit that reports what
it saw rather than deciding what it means.

What the browser is asked:

- **reachable** -- a plain request over the public URL, before a browser is
  worth starting. A preview that only answers from inside the machine is not a
  working preview, and that is the break this is here to catch.
- **loads** -- the browser navigated, the document came back under 400, the
  load event fired.
- **renders** -- the page painted something a user could see. An agent that
  breaks a build still serves HTTP 200, and a blank page is the most common
  regression there is.
- **no_page_errors** -- no uncaught exceptions.
- **resources** -- nothing the page asked for failed or 404'd. Chrome asks for
  a favicon whether or not a page references one, so that one is not counted.
- **console** -- no `console.error`.

Every run photographs the page, broken ones included: the screenshot is how a
person sees what the checks are describing.

**What it does not check is the intent.** The job carries what the workstream
set out to do, and the harness cannot tell whether it was done -- it checks
that the preview works, not that it works *as asked*. The report says so in as
many words rather than letting a green verification imply more than it means.
Closing that gap needs a judgement the harness does not have; the obvious
candidate is the agent itself, reading the screenshot.

The deployed image now carries chromium and the harness, so verification runs
for real in the container rather than being wired to nothing. What the
container still gives up is isolation: every profile shares it with the control
plane and with whatever services a workspace is running. `--build-arg
WITH_BROWSER=0` gets the old, much smaller image back.

## What has not been run against a real agent

Everything above was driven with a stand-in binary on `PATH` that echoes the
request, reports whether it resumed, and in streaming mode writes its answer a
fragment at a time. That exercises the plumbing -- argv, parsing, resuming,
accounting, throttled writes, the interface -- and proves none of the
behaviour of the real agent on a real repository.

The gap closes when a workspace runs on a machine with KVM. Until then, treat
every claim here as "the control plane does this correctly", not "the agent
does good work".

The verifier is no longer in that category. `cmd/dabberz-verify` is driven
end to end in its own tests -- a real browser, a real preview served over a
real socket, the report read back through `internal/verify` by way of a VM
driver -- so what it reports about a page is what a browser actually saw.


## Later: the preview zone, and a Mac as a build runner

Two decisions taken after the first draft of this document, recorded so the
reasoning is not lost.

**Previews live in their own hostname space, and are not access-controlled the
way the control plane is.** The control plane has a root shell behind it and
must be locked down; previews must not be, because the verifier's headless
browser would otherwise need a service token, and because a preview you cannot
open on your phone or send to someone is not serving its purpose.

A CDN access policy is not an origin control. If the origin IP is reachable, a
request carrying a forged Host header reaches the application and the policy
never sees it. Closing the origin (a tunnel, or a firewall limited to the CDN's
ranges) and authenticating inside the application are separate layers, and the
application layer is the only one that holds regardless of network path. That
is why authentication belongs in dabberz itself, behind a swappable
`Authenticator` interface, rather than being delegated to whatever sits in
front of it. The first implementation is a single-user signed session; a hosted
identity provider becomes a second implementation, which is also what makes
per-user GitHub and per-user agent credentials possible.

**A Mac as a build runner, for iOS and simulator work.** Deferred to v2, and
recorded here because it constrains the driver abstraction now. Firecracker
cannot run macOS, so a Mac runner is not another `vm.Driver` in the same sense
as the local and Firecracker drivers: it is a remote, long-lived, *unisolated*
machine that happens to be able to run Xcode and a simulator. The shape that
fits is a driver whose `Create` leases a workspace on a remote host over SSH
rather than booting anything, with the simulator standing in for the browser in
the verify loop. Nothing in the current abstraction forbids this -- `vm.Driver`
already hides whether an instance is a microVM or a directory, and
`vm.Interactive` is already an optional capability -- but it does mean resource
accounting and reclamation cannot assume the machine is disposable, which the
Firecracker path currently does.


## The preview edge: what works and what is blocked

The in-process router (`internal/proxy/hostmux`) is done and verified over
real HTTP: two stand-in dev servers behind two preview hostnames on one
listener, the public Host header preserved, the control-plane hostname still
serving the panel, an unmatched preview hostname a dead end rather than the
panel, and a stopped upstream returning an explanatory 502.

What is not working is the public edge *on the Dokploy-hosted instance*.
Dokploy stores a domain whose host is `*.ondabberz.link`, but the Traefik
router it generates uses `Host()`, which matches exact names only, so every
preview hostname falls through to Traefik's catch-all 404. Hand-editing the
per-application dynamic configuration to use `HostRegexp` -- in both the
Traefik v2 and v3 spellings -- changed nothing, while the panel's own router
in the same file kept working, which suggests that file is not what routes
these hostnames on this installation.

This matters less than it appears: that container has no `/dev/kvm`, so it
runs no fork VMs and has no previews to serve. The production shape -- dabberzd
on a machine with KVM, holding its own listener on 443 or sitting behind Caddy
-- has none of this problem.

The path that fits Dokploy's model, when it is worth doing, is a
`proxy.Router` that registers **one Dokploy domain per preview hostname**,
each pointed at the dabberz container's own port. dabberzd then routes
internally by Host exactly as it does now, so the adapter composes with
hostmux instead of replacing it, and no wildcard is needed.

That is blocked on authentication, not on plumbing. Dokploy attaches its
basic-auth middleware per *application*, so every domain on the app inherits
it -- and previews must not be authenticated. Replacing the proxy-level
password with authentication inside dabberzd removes the middleware entirely
and with it the conflict. So the order is: the `Authenticator` boundary first,
then drop the proxy password, then the per-hostname Dokploy router.


## Authentication: done, and what it unblocks

`internal/auth` is the boundary, with the provider behind an interface, and
Clerk is the first implementation. The important properties:

- Verification is in Go and frontend-agnostic, so the web application that
  replaces this interface reuses it unchanged. The sign-in code in `app.js` is
  the throwaway part and is deliberately minimal.
- RS256 only, as an allowlist. The attack a denylist misses is re-signing a
  token with HS256 using the RSA public key as the HMAC secret.
- Cookies are the primary channel, not a fallback: a browser cannot set headers
  on an EventSource or a WebSocket handshake, and the activity feed is
  server-sent events while the shell is a WebSocket.
- The middleware wraps the whole mux with a short list of exceptions, rather
  than being opted into per route, because the route somebody adds next would
  otherwise be open by default -- and one of these routes is a shell.
- Users are keyed by the provider's subject, not by email. An existing
  single-user installation is adopted on first sign-in rather than stranded.
- `auth.kind` has no default: a deployment that forgot to configure it fails to
  start instead of coming up open.

Only the publishable key is needed to verify a token -- it encodes the instance
host, which is where the issuer and signing keys come from. The secret key is
used solely to resolve a new user's email for attribution.

### The proxy password is gone

The deployed instance briefly carried an HTTP basic-auth password at the proxy,
as a stand-in while the application had no boundary of its own. It has been
removed, and that is a hardening rather than a loosening: a single static
credential, shared and unrotatable, is weaker than a session a provider issues
and expires, and it was redundant the moment the application started refusing
unidentified requests.

Verified afterwards with no credentials at all: the health check, the sign-in
configuration and the page itself answer, and every data route -- overview,
repositories, workspaces, the audit trail, the event stream and the shell --
returns 401. The retired password buys nothing. The page carries no data of its
own; it is a shell that reads the open sign-in configuration, sees it is not
signed in, and shows the form without requesting anything it would be refused.

This also removes the obstacle that made a per-hostname preview router
impossible: the proxy attached its password per application, so every hostname
on it inherited one, and previews must not be authenticated. What remains in
the way of the public preview edge is only the wildcard-routing problem
described above.

A development instance of an identity provider may also need this deployment's
origin added to it before sign-in completes from that hostname.


## Multiple users, each with their own GitHub

Two things had to be true before a second person could be let in, and neither
was.

**Tenancy was per-handler rather than per-route.** Authentication said who was
asking; what they were allowed to ask about was left to each handler
remembering to compare a user id, and several did not. Six routes served
anyone who knew an id, and `GET /v1/forks/{fork}/shell` was a terminal on the
machine holding somebody else's checkout. `POST /v1/tasks` took a repo id in
its body and never checked it at all.

The gate is now derived from the route pattern: a pattern naming `{repo}`,
`{task}`, `{fork}` or `{workspace}` is authorized before its handler runs, and
a route cannot opt out by forgetting. The test walks the registered route
table rather than a copy of it, so a route added later is covered the day it
is added. Something owned by somebody else is reported as missing rather than
forbidden, and a nonexistent id answers identically, so the two cannot be told
apart.

**One shared deploy token does not survive more than one person.** Whose
repository is cloned and what shows up in the commit history are per-account
questions. `internal/gitauth` resolves a git credential per user from the
identity provider, which already holds the GitHub connection. Nothing is
stored: no row, no vault entry, no file. Revoking the connection at GitHub
takes effect without dabberz having to notice.

The credential is handed to git and to nothing else. The repo's secrets are
injected into the machine and so are readable by the agent and by every
service a project runs; a person's GitHub token is not a repo secret and has
no business being there. A `GIT_TOKEN` set as a repo secret still wins, which
is what lets a repo be cloned by a machine account rather than by whoever
happened to open the workspace.

### The one thing only the account owner can fix

Clerk will not hand over a provider token while a connection uses its **shared
development OAuth credentials**. It answers
`oauth_shared_credentials_token_retrieval_not_supported`, which is what the
current `pk_test` instance will do. Making this work needs a GitHub OAuth app
of our own, registered and set as custom credentials for the GitHub connection
in the Clerk dashboard; connections made before that switch stay unretrievable
(`oauth_shared_credentials_stored_token_not_retrievable`), so each user
reconnects once afterwards. Private repositories additionally need the `repo`
scope on that app.

The account page says exactly this rather than reporting a generic failure,
because the symptom -- a GitHub account that is plainly connected and still
produces no token -- is otherwise unreadable.

## The look

The maroon came back. Both previous dabberz sites had the same one element: a
dark maroon band across the top with the wordmark and the burst on it. The
2007 one did it with gradients, bevels and highlights and the 2014 one did it
flat; this does it flat, because the page underneath is set in a serif and a
bevelled bar would fight it. The band is deliberately the only dark surface in
the interface, and the two status inks invert on it, since the cyan is
unreadable against maroon and the maroon would vanish into it.

## The pivot: from development platform to SaaS host

The product this was becoming — an agentic development platform — competes
directly with Lovable, Replit and v0, on a problem all of them are solving
badly: an agent starting from nothing, for a user who does not know what to
ask for.

The product it is becoming instead inverts that. A catalog of apps that replace
common per-seat SaaS — forms, links, a helpdesk — deployed in one click onto
dabberz infrastructure, pointed at the customer's own domain, and then modified
in English by an agent. Agent-on-a-known-small-codebase is a different
difficulty class from agent-on-greenfield: the blank page disappears, the
critical paths are known in advance so verification actually means something,
and rollback is correct because there is always a known-good baseline.

The fork → preview → verify → merge-gate pipeline, which was arguably
over-built for a development tool, turns out to be exactly the right shape for
"change a live app that takes real money". Almost nothing in the engine is
wasted. What changes is the surface, and what is missing is the operational
half: isolation, databases, backups, restore, export.

`docs/PLATFORM.md` is the contract that falls out of this, written before the
second app rather than after the fifth.

### The decisions taken

**Apps are built, not packaged.** Forking an existing open-source product means
maintaining thousands of divergent trees, each unable to take upstream's
security patches without risking a customer's changes. Agent capability was
never the constraint — upstream drift was. Building them means owning both
ends of that line: dabberz publishes the app as open source, and a customer's
fix flows back by pull request rather than into a private fork.

**The customer brings their own Claude, and dabberz never holds it.** Not
because it would be hard to bill tokens, but because it is the wrong business:
a margin race against the people who own the models, with no incentive to
optimise the agent workflows. "dabberz provides the environment, not the AI."

The mechanism matters legally. "Hand us your Claude credential" is the case the
terms above name explicitly. A hosted, unmodified binary that the customer
authenticates themselves, in their own isolated container, with the credential
never touching the control plane, is a different posture — and the same terms
contemplate it existing under Commercial Terms, which means there is a door to
ask about rather than a wall. Someone is already running that posture
commercially: the open-source Claude Code UI the design is modelled on also
powers a managed hosted product on bring-your-own-subscription. Precedent, not
permission.

So: ask, design so the answer is likely yes, and keep the credential layer
swappable so a no means a config change rather than a rewrite — which is what
this document already said to do.

**Per-tenant isolation moves onto the critical path.** It was stage 3. Once a
stranger's app runs next to another stranger's, with another customer's
attached Claude credential on the same box, Firecracker is table stakes for
both data and credentials.

**Agent access is free in every app.** Charging for it is the SSO tax wearing a
new hat, and one line on a pricing page would undo the entire position.

**Pricing is flat per app, unlimited seats.** Metered bandwidth is 2007 dabberz
muscle memory: the costs are not there any more, and an unpredictable bill is
precisely what this customer is running away from. "Unlimited seats" is the
wedge.

**Agentic on-call sells triage, not autonomy.** Diagnose, propose, run the
checks, ask. The moment an AI changes production unsupervised, dabberz owns
every outage it causes on somebody else's revenue.

**Scheduling is deferred**, on conflict-of-interest grounds rather than
technical ones. See the last section of `docs/PLATFORM.md`.

### What this costs

dabberz becomes an operations company. Downtime is a customer's lost revenue;
their customers' personal data makes dabberz a processor with the obligations
that carries; restores have to be tested rather than assumed. That work is the
unglamorous majority of what remains, and none of it is agent work.


### The blocks are the company

A refinement of the above, and a correction to it.

An earlier pass selected the catalog by what was cheap to host. That was the
wrong axis. Cheap to host is cheap for everyone, so those categories are
already commoditised and nobody is in pain over the price of a form builder.
The right axis is what the incumbent's price is paying for: almost always a
no-code configuration layer, which is exactly what an agent replaces.

The deeper correction is structural. The model is not a contract that apps
conform to, it is a set of **blocks** that apps are composed from — the Bending
Spoons move of putting every acquisition onto the same handful of core
services, applied to products built rather than bought. Data, identity,
secrets, mail, storage, jobs, payments, domains, realtime, webhooks, search,
import, agent access. Shared across every installation of every app, operated
once, amortised across everyone.

That changes which apps are possible. Realtime collaboration looked
unaffordable because a sync server per installation is an always-on process per
customer; one sync fabric with per-document rooms, shared across every app that
needs it, is a different arithmetic entirely. The first app to need it pays to
build it and every app after gets a connection string. Our Notion. Our Miro.

What does not change is the quality question, which is separate from the cost
one: an eighty-per-cent block editor is a tax paid every day by a team that
writes all day. The resolution is to replace a *use* rather than a logo. A wiki
that is read daily and edited weekly is a different product from a writing
tool, and the first is most of the market.

Three layers, kept straight: **blocks** are infrastructure, **components** are
the UI and domain parts that recur, **apps** are compositions. A thick app has
something in it that belongs a layer down. This is also why a custom app
builder is not a separate product — a template is a saved composition and a
custom app is one assembled on request, so the component library is the real
deliverable and the templates are its first customers. Extracted from real
apps, never designed ahead of them.

One block was missing from every earlier list and is the most on-brand thing
here: **import**. Nobody leaves HubSpot if leaving means retyping four thousand
contacts. Migration is the largest adoption blocker in every category in the
catalog and it is the same work every time. The product is an exit; an exit
needs a door at both ends.

### What it is

"The self-host for people who are not technical."

The objection that kills self-hosting for this buyer is never *can I run it* —
it is *do I want to be the person who runs it*. You built your own HubSpot;
now who patches it, who notices it is down, who restores it. dabberz is the
answer to that sentence, which makes the thing being sold operational
confidence rather than software. Everything in `docs/PLATFORM.md` that looks
like overhead — the check suite the agent cannot edit, promotion a human
performs, snapshots before migrations, tested restores, export as a feature —
is the product rather than a tax on it.

### Where the line actually is

The category now has company, which is worth being precise about rather than
dismissive of. Koru ships a notes app that is open source, local-first,
explicitly priced to work with the customer's own AI subscription rather than
reselling tokens, and not priced per seat. Three of those were positions taken
here first and held against the obvious objections, so a peer arriving at the
same ones is evidence rather than threat.

Which means **bring your own subscription is no longer a differentiator.**
Good: it was never meant to be one. It was meant to be the thing that stops
this becoming a reseller of somebody else's margin.

Two things in their shape are worth borrowing the opposite of.

**The pricing moved from per seat to per gigabyte, and that is not an
improvement.** It is the resource slice this project already refused in
`PLATFORM.md`: a bill that grows with the customer's own success, on an axis
they cannot predict and did not choose. Per seat taxes collaboration; per
gigabyte taxes keeping things. Both are a toll on using the product more, and
a customer who learns to delete things to control a bill has been made worse
at their job in a new way.

**"Self-hosted" and "we host it" are different products, and the gap is the
whole business.** Their headline offers self-hosting; the feature table says
the self-hosted sync server is coming. So today it means running it yourself,
which is precisely the objection this project exists to answer. Handing
somebody software is not handing them a working system, and the distance
between those two is where the operational confidence being sold actually
lives.

So the line is not *who builds it*, and "build your own SaaS" is the wrong
pitch for that reason — it puts the work back on the customer, which is the
same mistake as handing them a tarball, wearing different clothes. Four
positions, and only one of them leaves the customer holding neither job:

| | Runs it | Builds it | Owns the data |
|---|---|---|---|
| Incumbent SaaS | them | them | them |
| Self-hosted open source | **customer** | them | customer |
| Build-your-own | customer | **customer** | customer |
| dabberz | us | the agent | customer |

### The change flow is no longer novel either

Four days after the note above, Lovable shipped "drafts": copy a project, get
its own chat and preview, edit it apart from the live app, accept and then
publish, both by hand. That is this project's change flow, arrived at
independently, by somebody with distribution this does not have.

Worth being exact about what they built, because the differences are the
expensive parts rather than the obvious ones.

**A draft carries example data.** The post is explicit: every draft "comes
filled with example data", and anything added while testing "is cleared when
you accept". So a draft is a schema plus fixtures. A change here gets a fork
of the customer's real data, because a change tested against an empty database
has not been tested -- and the question a customer actually has is whether a
migration survives their forty thousand contacts, which fixtures cannot
answer. This is the slowest thing here to build and the slowest to copy.

**Nothing checks a draft.** Testing is described as looking at it. Here the
app's own critical-path suite runs, a real browser opens the preview at its
public hostname, promotion refuses a revision that moved since those passed,
and the result says out loud what was *not* verified. The difference between
"you can test it" and "it was tested" is the whole of `PLATFORM.md`.

**Rollback stops at publish.** They revert a draft before publishing; the post
says nothing about after. Here the previous revision is recorded, the database
is snapshotted before promoting, and going back is one request. That axis --
what happens when it breaks -- is the thing being sold.

And they are a builder: you start from nothing. This starts from something
that already works. Drafts do not answer "I need a HubSpot".

So the honest read is not that an idea was taken. It is that the rate of
arrival has gone up: two independent products landed on pieces of this thesis
inside a week. That is evidence the thesis is right and that the novelty in it
is nearly spent. What is left, and what neither of them has, is the part that
looks like overhead: the shared blocks, running the thing for somebody,
forking real data, checks that run themselves, and sending fixes back
upstream.

The defence was never the change flow. Somebody with distribution just shipped
it in a week.

And it is why import without export is the tell. Six import formats and no
export is a product that is easy to enter and quiet about leaving, which is
the posture of the thing being escaped from. Export is not a feature here, it
is the proof the promise is real: see `PLATFORM.md`, where an app's manifest
declares what it can take in and must be able to give back.
