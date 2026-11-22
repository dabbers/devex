# dabberz.com

The marketing site: static HTML and one stylesheet in `site/`, no build step,
deployed to Cloudflare Pages.

These notes live in `docs/` rather than in `site/` on purpose. Cloudflare Pages
serves every file in the build output directory, so anything left beside the
HTML is published -- this file would have been readable at
`dabberz.com/README.md`, including the note below about the legal pages not
having been reviewed.

Set in the same Broadsheet system as the control plane — the tokens in
`style.css` are copied from `internal/web/static/style.css` so the site and the
product look like the same thing. If the product's palette moves, move these.

## Deploying

Cloudflare Pages, connected to this repository:

- **Build command:** none
- **Build output directory:** `site`

The output directory is not optional. Left at its default the build scans the
repository root, finds a Go project and no HTML, and fails with
`Could not detect a directory containing static files`.

Functions live in `functions/` at the **repository root**, not under `site/`.
Pages looks for them relative to the project's root directory (the default,
`/`), while serving the build output directory beside them -- so
`functions/api/subscribe.js` is served at `/api/subscribe`. If the project's
root directory is ever changed to `site`, the functions directory has to move
with it.

No framework and no build means nothing to break between a commit and a deploy,
and nothing to keep up to date.

## Before it goes live

- `contact.html` points at `support@dab-media.com`, the catch-all that already
  serves Dab Media's other properties. Swap it when dabberz has its own
  mailbox; it appears twice, as the link and as the text.
- The two legal pages name Dab Media LLC and Delaware law. Both were written to
  describe what the platform actually does rather than from a template, but
  neither has been read by a lawyer. Have one read them before the service takes
  money.
- **The signup form has no endpoint.** `index.html` carries two
  `<form class="signup">` blocks — `#join` in the hero and `#join-foot` at the
  bottom — each with no `action`, and an HTML comment marking where it goes.
  Set the `action` on **both**.

  Until then a small script near the footer intercepts the submit, so a visitor
  who types an address is handed a prefilled `mailto:` instead of watching the
  page reload and lose it. The script disables itself the moment a form has an
  `action`, so adding the endpoint is the only change needed; delete the block
  and the `.signup-fallback` rule afterwards for tidiness.

  Cloudflare Pages has no backend by default. The cheapest real endpoint is a
  Pages Function under `functions/` writing to D1, which keeps the list on
  infrastructure that is already ours — worth something for a product sold on
  owning your own data.
