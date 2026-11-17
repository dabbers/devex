# Forms

The first app on the dabberz platform, and the one that proves the contract.

It is deliberately small: a form has a title and some questions, it has a
public address, and answers land in a table. What makes it worth building
first is not the feature set but that it touches every part of
[`docs/PLATFORM.md`](../../docs/PLATFORM.md) — a provisioned database,
identity it does not implement itself, declared secrets, a scheduled job, an
agent surface, an extension surface, and an export that round-trips.

## Running it

```sh
createdb forms
DATABASE_URL=postgres://localhost/forms?sslmode=disable \
FORMS_DEV_USER=you@example.com \
go run . serve
```

`FORMS_DEV_USER` is for local work only. In a real installation the platform
terminates identity and passes it on, and sets `DABBERZ_TRUSTED_PROXY=1` to
say that this app is unreachable except through it. Without one of the two the
admin surface serves no one, which is deliberate: an app that falls open when
identity is misconfigured is the worst failure available to it.

## The checks

```sh
FORMS_TEST_DATABASE_URL=postgres://localhost/forms_test?sslmode=disable ./checks/run.sh
```

These are the paths whose breakage costs a customer money or trust, not a
coverage target. The platform runs them from the pinned upstream version rather
than from an installation's working tree, so a change cannot rewrite the gate
it has to pass.

## What an agent may change

`theme/`, `config/`, `plugins/`, and tables prefixed `ext_`. Everything else is
core: the same bytes in every installation, so it can be upgraded in place. A
change that needs to touch core is not a customisation, it is a product bug,
and it goes upstream as a pull request rather than into a private fork.

## What is not done yet

- **Mail.** The digest job reports what it would have sent. The platform's mail
  block does not exist.
- **Identity.** Real OIDC, rather than a header the platform is trusted to set.
- **Storage.** No file upload field yet.
- **Destructive agent tools.** Everything the MCP server exposes is read-only,
  which sidesteps the confirmation rules rather than implementing them.
