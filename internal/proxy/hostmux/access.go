package hostmux

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/previewpass"
	"github.com/dabbers/devex/internal/proxy"
)

// Access at the edge.
//
// There are two places to decide. An app decides its own routes -- this form
// is for anyone, that one for members -- and on a 401 asks the edge to deal
// with whoever that is (proxy.SignInHeader), so no app writes a login. And
// each route carries a policy the edge keeps by itself, before the app sees
// the request: open for most sites, shut for a change's preview, which is
// code nobody has approved, and for any site its owner chose to lock.
//
// Refusing is only half of it. A member has to be able to get in, and the
// panel's sign-in cannot help directly: it lives on the control plane's
// domain, and its cookie deliberately never reaches the preview zone, where
// every app could read it. So the edge signs people in itself, by bouncing:
//
//  1. A visitor with no session asks for a members-only page. The edge sends
//     them to /_dabberz/signin on the same hostname.
//  2. That sets a state cookie on this hostname only and sends them to the
//     panel, naming the hostname and the state.
//  3. The panel, which knows who they are, asks this process for a one-time
//     code bound to that hostname, that state and that person, and sends them
//     back to /_dabberz/callback with it.
//  4. The callback redeems the code -- once -- checks the state cookie matches
//     what the code was issued for, and sets a session that only this hostname
//     receives.
//
// The state cookie is what stops someone signing a victim in as themselves: a
// code minted by an attacker is bound to the attacker's state, which the
// victim's browser does not have.

// reservedPrefix is the part of every app's path space the edge keeps for
// itself. Nothing under it reaches an app.
const reservedPrefix = "/_dabberz/"

// stateCookie carries the sign-in in progress between steps 2 and 4.
const stateCookie = "dabberz_signin"

// codeLifetime is how long a code from the panel is good for. The browser is
// redirected straight to the callback, so this only has to cover a slow
// network, and it is what bounds how long a leaked code is worth anything.
const codeLifetime = time.Minute

// sessionLifetime is how long a sign-in at one app lasts. When it runs out the
// next members-only page bounces again, which is invisible while the panel's
// own sign-in is still good.
const sessionLifetime = 12 * time.Hour

// Minter mints the session the edge sets at the end of a sign-in. It is
// previewpass.Issuer, narrowed, so a session and the verifier's pass are one
// thing to check.
type Minter interface {
	Mint(host string, user *domain.User, ttl time.Duration) (string, error)
}

// ErrUnknownHost means a sign-in was asked for at a hostname this edge does
// not serve. The panel would otherwise be redirecting people to wherever a
// link said.
var ErrUnknownHost = errors.New("hostmux: no app is published at that hostname")

// signIn is the bounce, when it is configured.
type signIn struct {
	panel  string // where step 2 sends people, without a trailing slash
	scheme string // how apps' own hostnames are reached
	mint   Minter

	mu    sync.Mutex
	codes map[string]handoff
	now   func() time.Time
}

// handoff is one code the panel has minted and the callback has not redeemed.
type handoff struct {
	host    string
	state   string
	user    domain.User
	expires time.Time
}

// WithSignIn lets members sign in at an app by way of the panel at panelURL.
//
// Without it a members-only page can only be refused, which is right for an
// edge with nowhere to send people and wrong anywhere else.
func (m *Mux) WithSignIn(panelURL, scheme string, mint Minter) *Mux {
	if scheme == "" {
		scheme = "https"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signIn = &signIn{
		panel:  strings.TrimRight(panelURL, "/"),
		scheme: scheme,
		mint:   mint,
		codes:  map[string]handoff{},
		now:    time.Now,
	}
	return m
}

// Handoff is step 3: the panel, having established who someone is, asks for a
// link that signs them in at host. The link is good once, for a minute, and
// only from the browser that started the sign-in.
func (m *Mux) Handoff(host, state string, user *domain.User) (string, error) {
	m.mu.RLock()
	s := m.signIn
	_, known := m.routes[normalizeHost(host)]
	m.mu.RUnlock()
	switch {
	case s == nil:
		return "", errors.New("hostmux: this edge does not sign people in")
	case !known:
		return "", ErrUnknownHost
	case state == "" || len(state) > 128:
		return "", errors.New("hostmux: a sign-in needs the state it was started with")
	case user == nil || user.ID == "":
		return "", errors.New("hostmux: a sign-in needs somebody to sign in")
	}
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	host = normalizeHost(host)

	s.mu.Lock()
	now := s.now()
	for c, h := range s.codes {
		if now.After(h.expires) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = handoff{host: host, state: state, user: *user, expires: now.Add(codeLifetime)}
	s.mu.Unlock()

	return s.scheme + "://" + host + reservedPrefix + "callback?code=" + url.QueryEscape(code), nil
}

// redeem takes a code out of circulation and returns what it was for. A code
// is spent by the attempt, not by success, so one that fails a check cannot
// be tried again with a different state.
func (s *signIn) redeem(code string) (handoff, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.codes[code]
	delete(s.codes, code)
	if !ok || s.now().After(h.expires) {
		return handoff{}, false
	}
	return h, true
}

// isMember reports whether who may reach a members-only path on this route.
// Members today are the owner; nobody is a member of a route with no owner.
func (rt *route) isMember(who domain.User, known bool) bool {
	return known && who.ID != "" && rt.owner != "" && who.ID == rt.owner
}

// cleanPath is the path an app will route, for judging against a policy. A
// visitor who asks for /f/../admin is asking for /admin.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

// membersOnly judges a request against the route's policy.
//
// The edge judges a decoded, cleaned path and forwards the one the visitor
// spelled, and an app may read that spelling differently: to a router,
// /admin/x%2F..%2F..%2Ff%2Fy is under /admin, and cleaned it is /f/y. So a
// path is only public when there is one way to read it. Any other is for
// members, whichever page it turns out to name.
func (rt *route) membersOnly(u *url.URL) bool {
	if rt.access.Mode == domain.AccessPublic {
		return false
	}
	return !unambiguous(u) || rt.access.MembersOnly(u.Path)
}

// unambiguous reports whether every app would route u to the path the edge
// judged: nothing to clean, no encoded separator or dot, nothing that decodes
// a second time, and none of the characters some servers treat as separators.
func unambiguous(u *url.URL) bool {
	if u.Path != cleanPath(u.Path) || strings.ContainsAny(u.Path, "\\;%?#\x00") {
		return false
	}
	raw := strings.ToLower(u.EscapedPath())
	return !strings.Contains(raw, "%2f") && !strings.Contains(raw, "%2e")
}

// serveRoute applies the route's policy and then forwards.
func (m *Mux) serveRoute(w http.ResponseWriter, r *http.Request, host string, rt *route) {
	if strings.HasPrefix(r.URL.Path, assetPrefix) {
		m.serveAsset(w, r, host, rt)
		return
	}
	if strings.HasPrefix(r.URL.Path, reservedPrefix) {
		m.serveEdge(w, r, host)
		return
	}
	r = m.withVisitor(r)
	shut := rt.membersOnly(r.URL)
	if shut {
		who, known := visitor(r.Context())
		if !rt.isMember(who, known) {
			m.refuse(w, r, host, who, known)
			return
		}
	}
	ctx := context.WithValue(r.Context(), forwardedKey{}, forwarded{asked: r, shut: shut})
	rt.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// forwarded is what the response side needs to know about a request the edge
// passed on: how it was asked, before rewriting, and whether the policy alone
// made it members-only.
type forwarded struct {
	asked *http.Request
	shut  bool
}

type forwardedKey struct{}

// errForMembers is an app's answer that the page is for members, given to
// somebody who is not one.
var errForMembers = errors.New("hostmux: the app says this is for its members")

// settle applies to an app's response what only the edge can: the sign-in it
// asked for, and keeping what a member was shown out of shared caches.
func (rt *route) settle(resp *http.Response) error {
	asked := resp.Header.Get(proxy.SignInHeader) != ""
	resp.Header.Del(proxy.SignInHeader)

	ctx := resp.Request.Context()
	who, known := visitor(ctx)
	member := rt.isMember(who, known)
	// Only for a non-member: bouncing a member the app refused would come
	// straight back here. An app's own 401, for accounts of its own, is the
	// app's and passes through.
	if asked && !member && resp.StatusCode == http.StatusUnauthorized {
		return errForMembers
	}
	if f, _ := ctx.Value(forwardedKey{}).(forwarded); member || f.shut {
		// The CDN-specific headers outrank Cache-Control where they are set.
		for _, name := range []string{"CDN-Cache-Control", "Cloudflare-CDN-Cache-Control", "Surrogate-Control", "Expires"} {
			resp.Header.Del(name)
		}
		resp.Header.Set("Cache-Control", "private, no-store")
	}
	return nil
}

// refuse answers a members-only request from somebody who is not a member.
func (m *Mux) refuse(w http.ResponseWriter, r *http.Request, host string, who domain.User, known bool) {
	m.mu.RLock()
	s := m.signIn
	m.mu.RUnlock()

	if known {
		// Signed in, and not a member. Sending them to sign in again would
		// loop; they need to be let in by whoever owns this.
		name := who.Email
		if name == "" {
			name = "an account"
		}
		page(w, http.StatusForbidden, "This is for members only",
			"You are signed in as "+name+", which is not one of this app's members. "+
				"Ask its owner to let you in, or sign in as somebody else.",
			reservedPrefix+"signout", "Sign out of this app")
		return
	}
	if s != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.Contains(r.Header.Get("Accept"), "text/html") {
		// A person in a browser: send them round the panel and back.
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, reservedPrefix+"signin?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	// A script, a form post or an edge with nowhere to send people. A
	// redirect to a sign-in page is not an answer a script can use.
	w.Header().Set("WWW-Authenticate", `Dabberz realm="`+host+`"`)
	page(w, http.StatusUnauthorized, "Sign in to continue",
		"This page belongs to the app's members.", reservedPrefix+"signin", "Sign in")
}

// serveEdge answers the paths the edge keeps for itself.
func (m *Mux) serveEdge(w http.ResponseWriter, r *http.Request, host string) {
	m.mu.RLock()
	s := m.signIn
	m.mu.RUnlock()

	switch strings.TrimPrefix(r.URL.Path, reservedPrefix) {
	case "signin":
		if s == nil {
			// Nowhere to sign in. The panel links here to open any app, so
			// carry on to the app as whoever they already are.
			http.Redirect(w, r, safeReturn(r.URL.Query().Get("return")), http.StatusFound)
			return
		}
		state, err := randomToken()
		if err != nil {
			http.Error(w, "could not start a sign-in", http.StatusInternalServerError)
			return
		}
		back := safeReturn(r.URL.Query().Get("return"))
		http.SetCookie(w, &http.Cookie{
			Name:     stateCookie,
			Value:    state + "." + base64.RawURLEncoding.EncodeToString([]byte(back)),
			Path:     reservedPrefix,
			MaxAge:   600,
			HttpOnly: true,
			Secure:   isHTTPS(r),
			SameSite: http.SameSiteLaxMode,
		})
		target := s.panel + "/#/open?host=" + url.QueryEscape(host) + "&state=" + url.QueryEscape(state)
		http.Redirect(w, r, target, http.StatusFound)

	case "callback":
		if s == nil {
			page(w, http.StatusNotFound, "Sign-in is not available here", "", "", "")
			return
		}
		h, ok := s.redeem(r.URL.Query().Get("code"))
		cookie, err := r.Cookie(stateCookie)
		var state, back string
		if err == nil {
			var encoded string
			state, encoded, _ = strings.Cut(cookie.Value, ".")
			if raw, err := base64.RawURLEncoding.DecodeString(encoded); err == nil {
				back = safeReturn(string(raw))
			}
		}
		switch {
		case !ok:
			m.logger.Warn("a sign-in code was not accepted", "host", host)
		case h.host != host:
			m.logger.Warn("a sign-in code was presented at the wrong app", "host", host, "issued_for", h.host)
			ok = false
		case state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(h.state)) != 1:
			// Not the browser that started this sign-in.
			m.logger.Warn("a sign-in code arrived without the state it was issued for", "host", host)
			ok = false
		}
		if !ok {
			page(w, http.StatusBadRequest, "That sign-in did not work",
				"The link was used already, took too long, or was started in another browser.",
				reservedPrefix+"signin", "Try again")
			return
		}
		session, err := s.mint.Mint(host, &h.user, sessionLifetime)
		if err != nil {
			m.logger.Error("could not mint a session", "host", host, "error", err)
			http.Error(w, "could not sign you in", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: reservedPrefix, MaxAge: -1})
		http.SetCookie(w, &http.Cookie{
			Name:     previewpass.CookieName,
			Value:    session,
			Path:     "/",
			MaxAge:   int(sessionLifetime / time.Second),
			HttpOnly: true,
			Secure:   isHTTPS(r),
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, back, http.StatusFound)

	case "signout":
		http.SetCookie(w, &http.Cookie{Name: previewpass.CookieName, Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/", http.StatusFound)

	default:
		http.NotFound(w, r)
	}
}

// safeReturn keeps a post-sign-in redirect on this hostname. Anything that is
// not a plain local path -- another host, a scheme, a protocol-relative
// "//elsewhere", a backslash some browsers read as a slash -- goes to the
// front page instead.
func safeReturn(back string) string {
	if back == "" || !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") ||
		strings.ContainsAny(back, "\\\r\n\t") || len(back) > 2048 ||
		strings.HasPrefix(back, reservedPrefix) {
		return "/"
	}
	return back
}

// isHTTPS reports whether the visitor reached us over TLS, directly or at the
// CDN in front. It only decides the Secure flag on a cookie, so trusting the
// forwarded header costs nothing if it lies.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// randomToken is 32 bytes of randomness, safe in a URL and a cookie.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("hostmux: no randomness available: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// page writes a small page of the edge's own. Everything in it is escaped:
// an email address is the visitor's to choose.
func page(w http.ResponseWriter, status int, title, body, href, link string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>%s</title><body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 16px">`+
		`<h1 style="font-size:1.4rem">%s</h1><p>%s</p>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
	if href != "" {
		fmt.Fprintf(w, `<p><a href="%s">%s</a></p>`, html.EscapeString(href), html.EscapeString(link))
	}
}

// dropReservedCookies stops an app setting a cookie in the edge's name. It
// could not forge a session -- those are signed -- but it could sign its
// visitor out, or plant a stale state to break their next sign-in.
func dropReservedCookies(header http.Header) {
	values := header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "dabberz_") {
			continue
		}
		header.Add("Set-Cookie", value)
	}
}
