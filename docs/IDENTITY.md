# Identity

Clerk was chosen before the pivot. It is the wrong shape now, and the reason
is not flexibility — it is that it contradicts the pricing promise and cannot
express the population the product now has to serve.

## Two populations, not one

Before the pivot there was one kind of person: the operator, signing in to a
development tool. Clerk is good at that.

The product now has two, and they have opposite requirements.

**Operators.** Who owns an installation, who may promote a change. Small
population — one per customer. High value per head. Wants the things a real
identity provider is good at: social sign-in, MFA, recovery, a hosted flow
nobody has to maintain.

**An app's own people.** Who may see this form's responses, submit a ticket,
appear in a CRM. Population is unbounded and belongs to the *customer*, not to
us. Must be isolated per installation. Must cost us nothing per head.

Clerk serves the first well and cannot serve the second at all.

## Why it cannot

**It contradicts the pricing promise.** Hosted identity is priced per monthly
active user. The platform's commitment is *never per person — we charge on
anything that isn't a human being*. If identity is billed per head, then every
customer's app growing its audience raises our cost linearly per human, which
is precisely the economics the pricing page refuses. We would be absorbing a
per-seat tax in order to promise we don't charge one. That does not survive
contact with a customer whose form gets popular. (Check current terms before
quoting numbers anywhere; the shape is what matters here, not the rate.)

**It conflates tenants.** Clerk authenticates *dabberz accounts*. An app's
users are not dabberz accounts. Serving them means either an instance per
installation — not how the product is provisioned or priced — or one pool with
organizations, which puts every customer's users in the same namespace. That
is the same class of mistake as announcing any signed-in visitor at the edge,
which has already been found and fixed once here. It is not worth
reintroducing a tenancy hazard to save writing a user table.

## What is already right

Two seams make this a smaller job than it sounds, both built before there was
a reason to need them.

`auth.Authenticator` is four methods — `Name`, `Identify`, `PublicConfig` —
and `internal/auth/clerk.go` is its only implementation. The provider is one
interface away from being swapped.

`identity.Translator` is deliberately the single place that answers "which
dabberz user is this request", precisely so the panel and the edge cannot
disagree. A second provider plugs in underneath it without touching either
front door.

## The shapes an app can ask for

Three of the four already exist as `domain.Access`:

| Shape | Today | Who the platform asserts |
|---|---|---|
| Internal only | `AccessInternal` | The owner. Everyone else is refused. |
| Public, no auth | `AccessPublic` | Nobody, though it may still recognise a member. |
| Mixed | `AccessHybrid` + public path prefixes | Members, except on the listed public paths. |
| **The app's own auth** | **new** | **Nothing. The app is on its own.** |

The fourth is nearly free: the edge strips identity headers as always and
asserts none, and offers no sign-in. Worth having as an escape hatch — and
worth noticing that an app in this mode is the *most* portable thing on the
platform, because it depends on no identity contract at all.

## Recommendation

**Keep a provider for operators. Own the app-user path.**

Operator sign-in stays with an IdP; the cost there is bounded by the number of
customers, not by their users, so per-head pricing is harmless. Whether that
stays Clerk is a smaller question and can be deferred.

For an app's own people, write it, and keep it boring:

- **Magic links only at launch.** No passwords stored means no hashing to get
  wrong, no credential stuffing, no breach lists, and no password-reset flow —
  which is the most-attacked part of any auth system and the easiest to get
  subtly wrong with one operator and no security team.
- **Sessions are signed cookies**, HMAC'd with the master key the vault
  already holds. No session store to keep consistent.
- **A user table per installation**, in the installation's own database.
  Tenancy then falls out of the architecture rather than being enforced by a
  query filter somebody can forget.
- **OIDC relying-party support later**, so an installation can delegate to
  Google Workspace, Microsoft or Okta when a customer asks.

### Why the user table belongs in the installation's database

The obvious counter-argument is that a central user table would let us offer
single sign-on across a customer's apps later. It does not survive looking at.

If the users and the issuer configuration live with the installation, then
**SSO is something the customer configures, per app, against their own
provider.** They point it at their Google Workspace or their Okta and it is
done. Nothing has to be built here, nothing has to be brokered here, and the
customer does not have to ask.

Cross-app single sign-on still happens — it just is not ours. A customer who
points three installations at one provider has SSO across all three, because
the shared thing is *their* directory rather than our user table. We get the
outcome without becoming an identity provider, which is a business we have no
reason to be in and could not price without breaking the per-person promise.

Three things follow, and all of them are good:

- **Tenancy is structural.** Separate databases, so one installation's users
  cannot be read from another by a query that forgot its filter.
- **An installation is self-contained**, which is the property that makes it
  ejectable at all.
- **Nobody needs a dabberz account to use a customer's app**, which keeps the
  per-head cost at zero on the only axis where it could have grown.

### Fork rather than write?

Not worth it. Forking Kratos, Zitadel, Authentik or similar means inheriting a
schema, an admin surface and an upgrade path that will never be taken, in
exchange for features this product does not have customers for yet — SAML,
enterprise directory sync, a policy engine. What is actually needed is a user
table, a magic link, a signed cookie and the three-header contract. That is
small enough to read in one sitting, which is the property that matters for
the component everything else trusts.

The argument for a fork is SSO for enterprise buyers. There are no enterprise
buyers. Buy that when somebody is asking to pay for it.

## Dependency, and a pattern worth noticing

Magic links need mail. Scoping the eject kit found that **mail and storage are
declared in manifests, refused at install time when absent, and then never
delivered to the app** — `install.Data.For()` returns `DATABASE_URL` and
nothing else.

That is now the second workstream blocked on the same gap. Wiring the mail
block is no longer a loose end; it is a prerequisite for both the identity
rework and an honest eject.

## Where identity configuration lives, and why not in app settings

`internal/appsettings` already has the right shape for app configuration: an
app declares its settings in `config/settings.json` in its own code, the agent
changes that file through review like any other change, and the platform
delivers the chosen values to `.dabberz/settings.json`, read-only. The
declaration is the app's; the values are the owner's.

Identity configuration sits on the platform side of that line, and the reason
is not tidiness:

**The agent writes `config/settings.json`.** If auth were declared there, an
agent editing an app could add, remove or alter authentication. The claim this
platform rests on is that the identity boundary sits underneath the app, where
an agent editing the app cannot reach it. A toggle labelled "require sign-in"
is one slipped review from being off. Secondarily, the component catalog has
no secret type, deliberately, and a client secret belongs in the vault under
the existing rule that values never reach the UI or the audit trail.

So the platform holds five things, none of them in the app's repository:

1. **Access mode** — internal, public, hybrid with its public prefixes, or the
   app's own. Already platform-owned (`domain.Access`, migration 0016).
2. **Who the members are** — the user table and invitations, in the
   installation's database.
3. **Which provider the installation trusts** — issuer, client ID.
4. **The client secret** — vault only.
5. **Self-serve rules** — invite-only, or an allowed email domain.

### The line, stated once

**The app decides which of its surfaces need a member. The platform decides
who counts as a member and how they prove it.**

"This form is members-only" is genuinely the app's business and belongs in
`config/settings.json`. "Members are people in this Google Workspace" is the
platform's. Same word, opposite sides of the boundary.

### One renderer, not two

None of this requires a second settings UI. The `appsettings` catalog is a
general declarative form system that happens to be fed by apps; the platform
can ship its own declaration for identity configuration against the same
catalog, same renderer, same validation, sourced from the platform rather than
from a repository the agent can write.

That needs one addition the catalog does not have: a **secret** component,
which writes to the vault and renders as "set" or "not set" rather than ever
displaying a value.

## Open questions

1. **Does the operator provider stay Clerk?** The Development-instance
   limitation already blocks per-account GitHub linking, and the leaked test
   key still wants rotating. Separate decision, but the answer may fall out of
   doing this work.
2. **Does an invited user of one installation become a dabberz account?** They
   must not, or the per-person cost comes back through the other door.
3. **Which side of the eject boundary does the issuer configuration fall?**
   It is platform-held, but it should travel with the installation so the
   eject kit carries it rather than handing somebody a README that says to set
   their provider up again. Platform-owned and installation-scoped, not
   platform-owned and centrally stored.
