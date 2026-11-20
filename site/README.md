# dabberz.com

The marketing site. Static HTML and one stylesheet, no build step, deployed to
Cloudflare Pages from this directory.

Set in the same Broadsheet system as the control plane — the tokens in
`style.css` are copied from `internal/web/static/style.css` so the site and the
product look like the same thing. If the product's palette moves, move these.

## Deploying

Cloudflare Pages, connected to this repository:

- **Build command:** none
- **Build output directory:** `site`

No framework and no build means nothing to break between a commit and a deploy,
and nothing to keep up to date.

## Before it goes live

- `contact.html` carries `REPLACE-ME@dabberz.com`. Put the real address in, in
  both the link and the text.
- The two legal pages name Dab Media LLC and Delaware law. Both were written to
  describe what the platform actually does rather than from a template, but
  neither has been read by a lawyer. Have one read them before the service takes
  money.
- `index.html`'s "Request access" button points at `contact.html`. Point it at a
  signup flow when there is one worth pointing at.
