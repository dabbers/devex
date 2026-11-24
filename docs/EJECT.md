# Eject

A command that turns a running installation into a directory somebody else can
run, and a target they can run it on. "Don't find value here? Here is your
export tool."

The point is not the marketing, though the marketing is good. Every simulated
prospect who read the landing page asked some version of *can I actually
leave*, and the strongest single sentence any of them said was about this:

> I would trust that single button more than all four of the "What we won't
> do" headings.

But the reason to build it early is that **it is a test of the contract, not a
feature on top of it.** If it works, portability is demonstrable rather than
claimed. If it doesn't, we have found lock-in we did not know about while
there is one app and one user, instead of after telling a thousand people the
software is theirs.

## What an ejected app actually needs

Forms declares five things in its manifest. Four of them port for free,
because they were specified as interfaces that already exist in the world:

| Block | Interface | Portable? |
|---|---|---|
| `database` | Postgres 16 | Yes. A dump restores onto any provider. |
| `storage` | S3-compatible (`endpoint`, `bucket`, `access_key`) | Yes. Tigris, R2, Spaces, MinIO. |
| `mail` | SMTP URL | Yes. Any provider. |
| `jobs` | cron expression | Yes. A cron container, or the target's scheduler. |
| `identity` | **ours** | **The only one that needs anything written.** |

That table is the architecture review. The only thing standing between an
installation and somebody else's machine is the one block we invented — and
see below, because the identity decision shrinks even that.

### The finding this scoping turned up

`install.Data.For()` returns exactly one variable, `DATABASE_URL`, and an
instance's environment is that plus the repo's vault secrets. **Mail and
storage are declared in the manifest, validated at install time, and never
delivered to the app.** An app that asked for them gets nothing.

So today there is less to eject than the manifest implies, and `requires:
mail/storage` is a promise the platform checks and does not keep. Wiring those
blocks is a prerequisite for an honest eject of Forms, and it is work that
needed doing anyway — eject just made it visible.

## The identity shim

The contract is three headers (`internal/proxy/proxy.go`):

- `X-Dabberz-User` — who is asking. Set by the edge, never by a visitor.
- `X-Dabberz-Email` — their address.
- `X-Dabberz-Signin` — the app's half. On a 401 it means *this page is for
  members*, and the edge decides whether to sign them in or refuse.

Everything inbound with the `X-Dabberz-` prefix is stripped before forwarding,
which is the only reason an app may trust what arrives.

A replacement is a small reverse proxy that:

1. strips every `X-Dabberz-*` header from the inbound request;
2. authenticates the visitor against any OIDC provider;
3. sets `User` and `Email` when they are signed in;
4. on a 401 carrying `Signin`, redirects to the provider instead of returning
   the 401;
5. enforces the installation's access mode — `public`, `internal`, or
   `hybrid` with its list of public path prefixes.

That is the deliverable. It is small — low hundreds of lines — and being small
is the point: it is the executable proof of the sentence already on the about
page, that the interface an app expects is "deliberately a boring one".

**It gets smaller still** once the identity rework lands (`docs/IDENTITY.md`).
If an installation's users and its issuer configuration live in the
installation's own database, the shim holds no state of its own: it reads the
same table and the same configuration out of the database that travelled with
the app. Nobody has to re-create accounts, re-invite anybody, or reconstruct
which provider the installation trusted.

That turns identity from the hard part of ejecting into very nearly the easy
part, and it is an argument for doing the identity work before the eject kit
rather than after.

It should carry the same test that the platform's own edge carries: a forged
`X-Dabberz-User` on an inbound request must not reach the app, proven by
deleting the strip and watching the test fail.

## Targets, in the order worth doing them

**1. `docker compose up`, anywhere.** No account, no vendor, runs offline,
and a sceptic can try it before trusting anything. This is the one that makes
the claim true; the others are convenience.

**2. Fly.io.** Maps onto the service model almost field for field — a long
-lived process on a port with a health path, Fly Postgres, Tigris for storage,
scheduled machines for the weekly digest. A `Dockerfile` and a `fly.toml`.

**3. Vercel — probably never, for this shape of app.** Forms is `go build -o
forms` then `./forms serve`: a long-lived process with a health check and a
cron job. Putting that on Vercel means rewriting the app, which defeats the
purpose of demonstrating it is portable. Render or Railway are the better
third target if one is wanted.

## Phases

**0. Eject Forms by hand.** No product code. Dump the database, write a
compose file, write the shim, bring it up in containers, sign in, confirm the
records are there. Half a day, and it decides whether anything below is worth
doing. Everything after this assumes it passed.

**1. The identity shim.** Its own repository rather than this one — "here are
the few hundred lines that replace us" is a better artifact standing alone.
Generic OIDC, with a worked Clerk example since that is what the platform
uses. One to two days.

**2. `dabberzctl eject <installation>`.** Emits a directory: the source at the
installation's current revision, a `pg_dump`, `compose.yml`, the shim's
configuration derived from the access policy and the manifest, an
`.env.example` listing the names of every secret the app needs, and a README.
**Secret names, never values** — the existing rule that values never leave the
vault applies here too. Two to three days.

**3. Prove it in CI.** A test that ejects a real installation, brings it up in
containers, drives it with a browser, and asserts the data is there and that
an anonymous visitor is refused. This is what keeps the promise true in six
months; without it the eject kit rots the first time the contract moves. One
day.

**4. Fly.io target.** `fly.toml`, `Dockerfile`, import instructions. One to
two days.

Phases 0–3 are roughly a week of focused work, and are the part that matters.

## Decisions needed before starting

1. **Wire mail and storage first, or eject without them?** Ejecting a Forms
   that never had working mail is honest but unimpressive. Wiring them is a
   day or so and makes the eject real.
2. **Where does the shim live?** Own repository is better for the story; this
   one is less work.
3. **What does the kit do about the agent?** An ejected app has no change
   flow — that is the thing the customer is giving up by leaving, and the
   README should say so plainly rather than quietly omitting it.
4. **Licence.** The shim is the piece most likely to be read by somebody
   deciding whether to trust the platform. It wants the permissive licence the
   platform is headed for, not AGPL.
