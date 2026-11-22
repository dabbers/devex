// POST /api/subscribe — the waitlist endpoint for dabberz.com.
//
// A Cloudflare Pages Function. Pages serves this from `functions/` at the
// project root; the static site is the build output directory alongside it.
//
// It needs a D1 binding named DB. Until that binding exists the handler says
// so plainly rather than throwing a 500, because a waitlist form that fails
// silently loses the only thing the page is for.

const MAX_EMAIL = 254; // RFC 5321 limit.

// Deliberately permissive. Address validation beyond "looks like an address"
// rejects real people, and the list is read by a human before it is used for
// anything.
const LOOKS_LIKE_EMAIL = /^[^@\s]+@[^@\s.]+\.[^@\s]+$/;

const json = (status, body) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8' },
  });

export async function onRequestPost({ request, env }) {
  if (!env.DB) {
    // Misconfiguration, not the visitor's fault: say so and keep the address
    // out of a log we are not sure exists.
    return json(503, { error: 'The list is not connected yet. Please email us instead.' });
  }

  let email = '';
  let trap = '';
  const type = request.headers.get('content-type') || '';
  try {
    if (type.includes('application/json')) {
      const body = await request.json();
      email = String(body.email || '');
      trap = String(body.company || '');
    } else {
      const form = await request.formData();
      email = String(form.get('email') || '');
      trap = String(form.get('company') || '');
    }
  } catch {
    return json(400, { error: 'That request could not be read.' });
  }

  // Honeypot: a field hidden from people and irresistible to bots. Answer 200
  // so the bot records a success and does not come back to try harder.
  if (trap.trim() !== '') return json(200, { ok: true });

  email = email.trim().toLowerCase();
  if (email.length > MAX_EMAIL || !LOOKS_LIKE_EMAIL.test(email)) {
    return json(400, { error: 'That does not look like an email address.' });
  }

  try {
    // INSERT OR IGNORE against a UNIQUE column: signing up twice is a
    // no-op rather than an error, and the caller is told the same thing
    // either way so the endpoint cannot be used to test whether an
    // address is already on the list.
    await env.DB.prepare(
      'INSERT OR IGNORE INTO waitlist (email, created_at, source) VALUES (?, ?, ?)'
    )
      .bind(email, new Date().toISOString(), (request.headers.get('referer') || '').slice(0, 300))
      .run();
  } catch (err) {
    return json(500, { error: 'We could not save that. Please email us instead.' });
  }

  return json(200, { ok: true });
}

// Anything that is not a POST.
export async function onRequest({ request }) {
  if (request.method === 'POST') return; // handled above
  return new Response('Method not allowed', { status: 405, headers: { allow: 'POST' } });
}
