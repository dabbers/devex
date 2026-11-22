package hostmux

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/previewpass"
	"github.com/dabbers/devex/internal/proxy"
)

// open is the policy of a route whose tests are about something else.
var open = domain.Access{Mode: domain.AccessPublic}

const (
	appHost = "intake.ondabberz.link"
	ownerID = "usr_owner"
)

var owner = &domain.User{ID: ownerID, Email: "owner@example.com"}

// recordingUpstream is an app that notes every request it is shown.
type recordingUpstream struct {
	mu   sync.Mutex
	seen []*http.Request
	// setCookies is what the app answers with.
	setCookies []string
	// status and headers are the rest of its answer, when a test cares.
	status  int
	headers http.Header
}

func (u *recordingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.seen = append(u.seen, r.Clone(context.Background()))
	u.mu.Unlock()
	for _, c := range u.setCookies {
		w.Header().Add("Set-Cookie", c)
	}
	for name, values := range u.headers {
		w.Header()[name] = values
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	io.WriteString(w, "app:"+r.URL.Path) //nolint:errcheck // a test upstream
}

func (u *recordingUpstream) requests() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.seen...)
}

// edgeFor is an edge serving one app at appHost under policy, owned by
// ownerID, with sign-in through a panel. who is the panel-domain identity the
// edge would otherwise find; nil for nobody.
func edgeFor(t *testing.T, policy domain.Access, who *domain.User) (*Mux, *recordingUpstream) {
	t.Helper()
	app := &recordingUpstream{}
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	passes, err := previewpass.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	mux := New("ondabberz.link", &stubVisitors{as: who}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux.WithPasses(passes).WithSignIn("https://panel.example.com", "https", passes)
	if err := mux.Apply(context.Background(), []proxy.Route{{
		Hostname:     appHost,
		UpstreamHost: "127.0.0.1",
		UpstreamPort: upstreamPort(t, server.URL),
		OwnerID:      ownerID,
		Access:       policy,
	}}); err != nil {
		t.Fatal(err)
	}
	return mux, app
}

// visit asks the edge for target at appHost as a browser would.
func visit(mux http.Handler, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+appHost+target, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("X-Forwarded-Proto", "https")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var internal = domain.Access{Mode: domain.AccessInternal}

// TestAMembersOnlyPageIsKeptFromEveryoneButMembers: the app behind an
// internal route is never shown a request from somebody who is not a member,
// whatever they are -- a person in a browser, a script, or somebody signed in
// who is not one of its members.
func TestAMembersOnlyPageIsKeptFromEveryoneButMembers(t *testing.T) {
	t.Parallel()

	t.Run("a person who has not signed in is sent to sign in", func(t *testing.T) {
		mux, app := edgeFor(t, internal, nil)
		rec := visit(mux, http.MethodGet, "/forms/1?tab=all")
		if rec.Code != http.StatusFound {
			t.Fatalf("status %d, want a redirect to sign in", rec.Code)
		}
		if got, want := rec.Header().Get("Location"), "/_dabberz/signin?return="+url.QueryEscape("/forms/1?tab=all"); got != want {
			t.Errorf("sent to %q, want %q", got, want)
		}
		if n := len(app.requests()); n != 0 {
			t.Errorf("the app was shown %d requests from somebody not signed in", n)
		}
	})

	t.Run("a script is refused rather than redirected", func(t *testing.T) {
		mux, app := edgeFor(t, internal, nil)
		req := httptest.NewRequest(http.MethodPost, "http://"+appHost+"/mcp", strings.NewReader("{}"))
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
		if n := len(app.requests()); n != 0 {
			t.Errorf("the app was shown %d requests", n)
		}
	})

	t.Run("somebody signed in who is not a member is turned away", func(t *testing.T) {
		stranger := &domain.User{ID: "usr_stranger", Email: "<b>stranger</b>@example.com"}
		mux, app := edgeFor(t, internal, stranger)
		rec := visit(mux, http.MethodGet, "/")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403; a redirect to sign in again would loop", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<b>") {
			t.Error("the visitor's email reached the page unescaped")
		}
		if n := len(app.requests()); n != 0 {
			t.Errorf("the app was shown %d requests from a non-member", n)
		}
	})

	t.Run("the owner gets through and is named to the app", func(t *testing.T) {
		mux, app := edgeFor(t, internal, owner)
		rec := visit(mux, http.MethodGet, "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		seen := app.requests()
		if len(seen) != 1 || seen[0].Header.Get(proxy.UserHeader) != ownerID {
			t.Errorf("the app was not told the owner is asking")
		}
	})
}

// TestAHybridAppIsOpenOnlyWhereItSays: a form anyone can fill in, on an app
// only its members can manage. The public list is a list of prefixes, judged
// against the path the app will actually route.
func TestAHybridAppIsOpenOnlyWhereItSays(t *testing.T) {
	t.Parallel()
	forms := domain.Access{Mode: domain.AccessHybrid, Public: []string{"/f/", "/healthz"}}

	for path, public := range map[string]bool{
		"/f/contact":        true,
		"/f/":               true,
		"/f":                true,
		"/healthz":          true,
		"/healthz/":         false,
		"/healthzz":         false,
		"/":                 false,
		"/forms/1":          false,
		"/fx":               false,
		"/f/../forms/1":     false,
		"/f/%2e%2e/forms/1": false,
		"/export":           false,
	} {
		mux, app := edgeFor(t, forms, nil)
		rec := visit(mux, http.MethodGet, path)
		reached := len(app.requests()) > 0
		if reached != public {
			t.Errorf("%s: reached the app = %v (status %d), want %v", path, reached, rec.Code, public)
		}
	}
}

// signInAs walks the whole bounce for a browser that asked for target, as the
// panel vouching for user. It returns the session cookie and where the browser
// ended up.
func signInAs(t *testing.T, mux *Mux, target string, user *domain.User) (*http.Cookie, string) {
	t.Helper()

	first := visit(mux, http.MethodGet, target)
	start := visit(mux, http.MethodGet, first.Header().Get("Location"))
	if start.Code != http.StatusFound {
		t.Fatalf("starting the sign-in: status %d", start.Code)
	}
	toPanel, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if toPanel.Host != "panel.example.com" {
		t.Fatalf("sent to %s, want the panel", toPanel)
	}
	fragment, err := url.ParseQuery(strings.TrimPrefix(toPanel.Fragment, "/open?"))
	if err != nil {
		t.Fatal(err)
	}
	if fragment.Get("host") != appHost {
		t.Fatalf("the panel was told the app is %q", fragment.Get("host"))
	}
	state := cookieNamed(start, stateCookie)
	if state == nil || !state.HttpOnly || !state.Secure {
		t.Fatalf("the sign-in state cookie is %+v; it must be HttpOnly and Secure", state)
	}

	link, err := mux.Handoff(appHost, fragment.Get("state"), user)
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	back, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if back.Scheme != "https" || back.Host != appHost {
		t.Fatalf("the panel would send the browser to %s", link)
	}
	done := visit(mux, http.MethodGet, back.RequestURI(), state)
	if done.Code != http.StatusFound {
		t.Fatalf("callback: status %d: %s", done.Code, done.Body)
	}
	session := cookieNamed(done, previewpass.CookieName)
	if session == nil || !session.HttpOnly || !session.Secure || session.Domain != "" {
		t.Fatalf("the session cookie is %+v; it must be HttpOnly, Secure and host-only", session)
	}
	return session, done.Header().Get("Location")
}

// TestAMemberSignsInByWayOfThePanel is the whole loop: refused, sent round the
// panel, back with a session, and let in -- with the session itself never
// shown to the app.
func TestAMemberSignsInByWayOfThePanel(t *testing.T) {
	t.Parallel()
	mux, app := edgeFor(t, internal, nil)

	session, landed := signInAs(t, mux, "/forms/1?tab=all", owner)
	if landed != "/forms/1?tab=all" {
		t.Errorf("after signing in the browser went to %q, want the page it asked for", landed)
	}

	rec := visit(mux, http.MethodGet, "/forms/1", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("with a session: status %d", rec.Code)
	}
	seen := app.requests()
	if len(seen) != 1 {
		t.Fatalf("the app saw %d requests, want 1", len(seen))
	}
	if got := seen[0].Header.Get(proxy.UserHeader); got != ownerID {
		t.Errorf("the app was told the visitor is %q", got)
	}
	if got := seen[0].Header.Get(proxy.EmailHeader); got != owner.Email {
		t.Errorf("the app was told the email is %q", got)
	}
	if strings.Contains(seen[0].Header.Get("Cookie"), "dabberz_") {
		t.Errorf("the app was shown the edge's cookies: %s", seen[0].Header.Get("Cookie"))
	}
}

// TestSigningInAsANonMemberGetsARefusalNotALoop: the panel vouches for who
// somebody is, and that is all. It does not make them a member.
func TestSigningInAsANonMemberGetsARefusalNotALoop(t *testing.T) {
	t.Parallel()
	mux, app := edgeFor(t, internal, nil)
	session, _ := signInAs(t, mux, "/", &domain.User{ID: "usr_stranger", Email: "s@example.com"})
	if rec := visit(mux, http.MethodGet, "/", session); rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", rec.Code)
	}
	if n := len(app.requests()); n != 0 {
		t.Errorf("the app was shown %d requests", n)
	}
}

// TestASignInCodeIsGoodOnceForOneBrowserAtOneApp covers each way a code from
// the panel could be misused. The one that matters most is the missing state:
// without it, anybody could get a code for their own account and send a
// victim's browser to the callback, signing the victim in as them.
func TestASignInCodeIsGoodOnceForOneBrowserAtOneApp(t *testing.T) {
	t.Parallel()

	start := func(t *testing.T, mux *Mux) (state *http.Cookie, value string) {
		t.Helper()
		rec := visit(mux, http.MethodGet, "/_dabberz/signin?return=/")
		state = cookieNamed(rec, stateCookie)
		value, _, _ = strings.Cut(state.Value, ".")
		return state, value
	}
	callback := func(t *testing.T, link string) string {
		t.Helper()
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		return u.RequestURI()
	}
	refused := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusBadRequest || cookieNamed(rec, previewpass.CookieName) != nil {
			t.Errorf("status %d with a session %v; want a refusal and no session",
				rec.Code, cookieNamed(rec, previewpass.CookieName))
		}
	}

	t.Run("without the browser's state", func(t *testing.T) {
		mux, _ := edgeFor(t, internal, nil)
		_, value := start(t, mux)
		link, err := mux.Handoff(appHost, value, owner)
		if err != nil {
			t.Fatal(err)
		}
		refused(t, visit(mux, http.MethodGet, callback(t, link)))
	})

	t.Run("with somebody else's state", func(t *testing.T) {
		mux, _ := edgeFor(t, internal, nil)
		victim, _ := start(t, mux)
		_, attacker := start(t, mux)
		link, err := mux.Handoff(appHost, attacker, owner)
		if err != nil {
			t.Fatal(err)
		}
		refused(t, visit(mux, http.MethodGet, callback(t, link), victim))
	})

	t.Run("twice", func(t *testing.T) {
		mux, _ := edgeFor(t, internal, nil)
		state, value := start(t, mux)
		link, err := mux.Handoff(appHost, value, owner)
		if err != nil {
			t.Fatal(err)
		}
		if rec := visit(mux, http.MethodGet, callback(t, link), state); rec.Code != http.StatusFound {
			t.Fatalf("first use: status %d", rec.Code)
		}
		refused(t, visit(mux, http.MethodGet, callback(t, link), state))
	})

	t.Run("after a failed attempt", func(t *testing.T) {
		mux, _ := edgeFor(t, internal, nil)
		state, value := start(t, mux)
		link, err := mux.Handoff(appHost, value, owner)
		if err != nil {
			t.Fatal(err)
		}
		visit(mux, http.MethodGet, callback(t, link)) // no state: spent
		refused(t, visit(mux, http.MethodGet, callback(t, link), state))
	})

	t.Run("late", func(t *testing.T) {
		mux, _ := edgeFor(t, internal, nil)
		state, value := start(t, mux)
		link, err := mux.Handoff(appHost, value, owner)
		if err != nil {
			t.Fatal(err)
		}
		mux.signIn.now = func() time.Time { return time.Now().Add(codeLifetime + time.Second) }
		refused(t, visit(mux, http.MethodGet, callback(t, link), state))
	})

	t.Run("at another app", func(t *testing.T) {
		mux, _ := edgeFor(t, internal, nil)
		other := "other.ondabberz.link"
		routes := []proxy.Route{
			{Access: internal, Hostname: appHost, UpstreamHost: "127.0.0.1", UpstreamPort: 1, OwnerID: ownerID},
			{Access: internal, Hostname: other, UpstreamHost: "127.0.0.1", UpstreamPort: 1, OwnerID: ownerID},
		}
		if err := mux.Apply(context.Background(), routes); err != nil {
			t.Fatal(err)
		}
		state, value := start(t, mux)
		link, err := mux.Handoff(other, value, owner)
		if err != nil {
			t.Fatal(err)
		}
		// The code for the other app, presented here with a matching state.
		refused(t, visit(mux, http.MethodGet, callback(t, link), state))
	})
}

// TestThePanelOnlySendsPeopleToAppsThisEdgeServes: the handoff is a redirect
// the panel performs, so a hostname that is not one of ours would make the
// panel an open redirect carrying a code.
func TestThePanelOnlySendsPeopleToAppsThisEdgeServes(t *testing.T) {
	t.Parallel()
	mux, _ := edgeFor(t, internal, nil)
	if _, err := mux.Handoff("evil.example.com", "state", owner); err != ErrUnknownHost {
		t.Errorf("Handoff to a hostname nobody publishes: %v, want ErrUnknownHost", err)
	}
}

// TestAfterSigningInTheBrowserStaysOnTheApp: where to go afterwards rides
// through the bounce, and must not be a way off the app.
func TestAfterSigningInTheBrowserStaysOnTheApp(t *testing.T) {
	t.Parallel()
	for back, want := range map[string]string{
		"/forms/1?x=y":             "/forms/1?x=y",
		"//evil.example.com/":      "/",
		"/\\evil.example.com":      "/",
		"https://evil.example.com": "/",
		"/_dabberz/signin":         "/",
		"":                         "/",
	} {
		if got := safeReturn(back); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", back, got, want)
		}
	}

	mux, _ := edgeFor(t, internal, nil)
	rec := visit(mux, http.MethodGet, "/_dabberz/signin?return="+url.QueryEscape("//evil.example.com/"))
	state := cookieNamed(rec, stateCookie)
	value, _, _ := strings.Cut(state.Value, ".")
	link, err := mux.Handoff(appHost, value, owner)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	done := visit(mux, http.MethodGet, u.RequestURI(), state)
	if got := done.Header().Get("Location"); got != "/" {
		t.Errorf("after signing in the browser was sent to %q", got)
	}
}

// TestTheEdgesPathsAndCookiesAreNotTheApps: nothing under /_dabberz/ reaches
// an app, and an app cannot set a cookie in the edge's name -- it could not
// forge a session, but it could sign its visitor out or plant a stale state.
func TestTheEdgesPathsAndCookiesAreNotTheApps(t *testing.T) {
	t.Parallel()
	mux, app := edgeFor(t, open, nil)
	app.setCookies = []string{
		"dabberz_preview_pass=planted; Path=/",
		"DABBERZ_SIGNIN=planted; Path=/_dabberz/",
		"theirs=kept; Path=/",
	}

	if rec := visit(mux, http.MethodGet, "/_dabberz/whatever"); rec.Code != http.StatusNotFound {
		t.Errorf("/_dabberz/whatever: status %d, want 404 from the edge", rec.Code)
	}
	if n := len(app.requests()); n != 0 {
		t.Errorf("the app was shown the edge's own path")
	}

	rec := visit(mux, http.MethodGet, "/")
	var names []string
	for _, c := range rec.Result().Cookies() {
		names = append(names, c.Name)
	}
	if len(names) != 1 || names[0] != "theirs" {
		t.Errorf("cookies set through the edge: %v, want only the app's own", names)
	}
}

// TestSigningOutEndsTheSession is the way out of being signed in as the wrong
// person.
func TestSigningOutEndsTheSession(t *testing.T) {
	t.Parallel()
	mux, _ := edgeFor(t, internal, nil)
	rec := visit(mux, http.MethodGet, "/_dabberz/signout")
	c := cookieNamed(rec, previewpass.CookieName)
	if c == nil || c.MaxAge >= 0 {
		t.Errorf("signing out left the session cookie: %+v", c)
	}
}

// TestAPathTwoWaysToReadIsNeverPublic: the edge forwards the path as it was
// spelled, so one that cleans to a public page and routes to an admin one
// must not get through. Nor may the reverse.
func TestAPathTwoWaysToReadIsNeverPublic(t *testing.T) {
	t.Parallel()
	forms := domain.Access{Mode: domain.AccessHybrid, Public: []string{"/f/"}}

	for target, public := range map[string]bool{
		"/admin/x%2F..%2F..%2Ff%2Fy": false,
		"/admin/x%2f..%2f..%2ff%2fy": false,
		"/admin/../f/y":              false,
		"/admin/%2e%2e/f/y":          false,
		"/f/%252e%252e/admin":        false,
		"/f/..;/admin":               false,
		"/f/..%5cadmin":              false,
		"/f//admin":                  false,
		"/f/%2e":                     false,
		"/f/a%2Fb":                   false,
		"/f/v1%2e2":                  false,
		"/f/x%00":                    false,
		"/f/x%23y":                   false,
		"/f/x%3Fy":                   false,
		"/f/contact":                 true,
		"/f/contact%20us":            true,
		"/f/v1.2":                    true,
		"/f/contact?next=%2Fthanks":  true,
	} {
		mux, app := edgeFor(t, forms, nil)
		rec := visit(mux, http.MethodGet, target)
		if reached := len(app.requests()) > 0; reached != public {
			t.Errorf("%s: reached the app = %v (status %d), want %v", target, reached, rec.Code, public)
		}
	}
}

// TestTheOwnerCanSignInAtAPublicApp: a public app refuses nobody, so nothing
// bounces its owner, and Forms met them as a stranger. The sign-in path works
// whatever the policy, which is what the panel's Open links rely on.
func TestTheOwnerCanSignInAtAPublicApp(t *testing.T) {
	t.Parallel()
	mux, app := edgeFor(t, open, nil)

	start := visit(mux, http.MethodGet, "/_dabberz/signin?return=/")
	state := cookieNamed(start, stateCookie)
	if state == nil {
		t.Fatalf("a public app did not start a sign-in: status %d", start.Code)
	}
	value, _, _ := strings.Cut(state.Value, ".")
	link, err := mux.Handoff(appHost, value, owner)
	if err != nil {
		t.Fatal(err)
	}
	back, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	session := cookieNamed(visit(mux, http.MethodGet, back.RequestURI(), state), previewpass.CookieName)
	if session == nil {
		t.Fatal("no session at a public app")
	}
	visit(mux, http.MethodGet, "/", session)
	if seen := app.requests(); len(seen) != 1 || seen[0].Header.Get(proxy.UserHeader) != ownerID {
		t.Error("a public app was not told its owner is asking")
	}

	// With nowhere to sign in, the link still opens the app.
	mux.signIn = nil
	if rec := visit(mux, http.MethodGet, "/_dabberz/signin?return=/forms/1"); rec.Header().Get("Location") != "/forms/1" {
		t.Errorf("with no panel the Open link ends at %q, status %d", rec.Header().Get("Location"), rec.Code)
	}
}

// TestAnAppAsksForSignInAndTheEdgeDoesIt: on an open site the app decides a
// page is for members, and the edge does what follows for each kind of
// visitor, without ever looping.
func TestAnAppAsksForSignInAndTheEdgeDoesIt(t *testing.T) {
	t.Parallel()
	asking := http.Header{proxy.SignInHeader: {"required"}}
	stranger := &domain.User{ID: "usr_other", Email: "other@example.com"}

	for name, c := range map[string]struct {
		who      *domain.User
		status   int
		headers  http.Header
		method   string
		accept   string
		want     int
		location string
	}{
		"a person signed out": {nil, 401, asking, "GET", "text/html", 302,
			"/_dabberz/signin?return=" + url.QueryEscape("/f/team?x=1&y=2")},
		"a script":                         {nil, 401, asking, "GET", "application/json", 401, ""},
		"a post":                           {nil, 401, asking, "POST", "text/html", 401, ""},
		"somebody who is not a member":     {stranger, 401, asking, "GET", "text/html", 403, ""},
		"a member the app still refused":   {owner, 401, asking, "GET", "text/html", 401, ""},
		"the app's own accounts":           {nil, 401, nil, "GET", "text/html", 401, ""},
		"the header on a page that loaded": {nil, 200, asking, "GET", "text/html", 200, ""},
	} {
		mux, app := edgeFor(t, open, c.who)
		app.status, app.headers = c.status, c.headers
		req := httptest.NewRequest(c.method, "http://"+appHost+"/f/team?x=1&y=2", nil)
		req.Header.Set("Accept", c.accept)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != c.want || rec.Header().Get("Location") != c.location {
			t.Errorf("%s: %d to %q, want %d to %q", name, rec.Code, rec.Header().Get("Location"), c.want, c.location)
		}
		if rec.Header().Get(proxy.SignInHeader) != "" {
			t.Errorf("%s: the app's request to the edge reached the visitor", name)
		}
		replaced := c.who != owner && c.status == 401 && c.headers != nil
		if shown := strings.Contains(rec.Body.String(), "app:"); shown == replaced {
			t.Errorf("%s: the app's page shown = %v", name, shown)
		}
		if c.want == http.StatusFound && rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: the bounce can be cached", name)
		}
	}
}

// TestWhatAMemberWasShownIsNotCached: the CDN caches by path, so a members'
// file it kept would be served to whoever asked next.
func TestWhatAMemberWasShownIsNotCached(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		policy  domain.Access
		who     *domain.User
		private bool
	}{
		"a member at an open site":   {open, owner, true},
		"a member at a shut route":   {internal, owner, true},
		"a stranger at an open site": {open, nil, false},
	} {
		mux, app := edgeFor(t, c.policy, c.who)
		app.headers = http.Header{
			"Cache-Control":                {"public, max-age=14400"},
			"Cdn-Cache-Control":            {"max-age=14400"},
			"Cloudflare-Cdn-Cache-Control": {"max-age=14400"},
		}
		got := visit(mux, http.MethodGet, "/app.js").Header()
		private := got.Get("Cache-Control") == "private, no-store" &&
			got.Get("CDN-Cache-Control") == "" && got.Get("Cloudflare-CDN-Cache-Control") == ""
		if private != c.private {
			t.Errorf("%s: kept out of shared caches = %v, want %v (%v)", name, private, c.private, got)
		}
	}
}
