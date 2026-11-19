package hostmux

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/proxy"
)

// upstream starts a server standing in for a fork's dev server and returns its
// host and port.
func upstream(t *testing.T, handler http.Handler) (string, int) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse upstream port %q: %v", parsed.Port(), err)
	}
	return parsed.Hostname(), port
}

func get(t *testing.T, h http.Handler, host, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestRoutesByHostAndFallsBackToTheControlPlane(t *testing.T) {
	host, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "fork dev server: "+r.URL.Path)
	}))

	mux := New("ondabberz.link", nil, nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "control plane")
	}))
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := body(t, get(t, mux, "web-shop-cart.ondabberz.link", "/checkout")); got != "fork dev server: /checkout" {
		t.Fatalf("preview hostname did not reach the fork: %q", got)
	}
	// A hostname outside the preview zone is the control plane.
	if got := body(t, get(t, mux, "dabberz.dab.im", "/")); got != "control plane" {
		t.Fatalf("control-plane hostname was not served: %q", got)
	}
}

// TestUnknownPreviewHostnameNeverReachesTheControlPlane is the security
// property of sharing one listener between the panel and the previews.
//
// Whatever sits in front authenticates the control plane's hostname and
// deliberately leaves the preview zone open, so if an unmatched preview
// hostname fell through, the panel would be served without authentication at
// any name under the preview domain.
func TestUnknownPreviewHostnameNeverReachesTheControlPlane(t *testing.T) {
	mux := New("ondabberz.link", nil, nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "control plane")
	}))
	if err := mux.Apply(context.Background(), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, host := range []string{
		"ondabberz.link",
		"anything.ondabberz.link",
		"web-shop-cart.ondabberz.link",
		"DEEP.NESTED.ondabberz.link",
	} {
		resp := get(t, mux, host, "/")
		text := body(t, resp)
		if strings.Contains(text, "control plane") {
			t.Fatalf("%s served the control plane without authentication", host)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", host, resp.StatusCode)
		}
		// Preview URLs outlive their forks, so a stale link is expected and
		// deserves an explanation rather than a bare status.
		if !strings.Contains(text, "No preview is published") {
			t.Fatalf("%s did not explain itself: %q", host, text)
		}
	}
}

// TestLookalikeDomainIsNotTreatedAsThePreviewZone guards the suffix check: a
// naive strings.HasSuffix would match a domain somebody else controls.
func TestLookalikeDomainIsNotTreatedAsThePreviewZone(t *testing.T) {
	mux := New("ondabberz.link", nil, nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "control plane")
	}))

	// Not subdomains of ondabberz.link, so they are not previews.
	for _, host := range []string{"notondabberz.link", "evil-ondabberz.link"} {
		if got := body(t, get(t, mux, host, "/")); got != "control plane" {
			t.Fatalf("%s was wrongly treated as a preview hostname: %q", host, got)
		}
	}
}

func TestPreservesThePublicHostname(t *testing.T) {
	var seen string
	host, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Host
		io.WriteString(w, "ok")
	}))

	mux := New("ondabberz.link", nil, nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	get(t, mux, "web-shop-cart.ondabberz.link", "/")

	// A dev server derives absolute URLs and its hot-reload websocket address
	// from the Host it was asked for. Rewriting it to the internal address
	// makes the page load and then fail to connect.
	if seen != "web-shop-cart.ondabberz.link" {
		t.Fatalf("upstream saw Host %q, want the public hostname", seen)
	}
}

func TestMatchesHostRegardlessOfPortAndCase(t *testing.T) {
	host, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "reached")
	}))

	mux := New("ondabberz.link", nil, nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "fallback")
	}))
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, spelling := range []string{
		"web-shop-cart.ondabberz.link",
		"WEB-SHOP-CART.ondabberz.link",
		"web-shop-cart.ondabberz.link:443",
	} {
		if got := body(t, get(t, mux, spelling, "/")); got != "reached" {
			t.Fatalf("Host %q did not match the route: %q", spelling, got)
		}
	}
}

func TestApplyReplacesTheWholeTable(t *testing.T) {
	host, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "reached")
	}))

	mux := New("ondabberz.link", nil, nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "fallback")
	}))
	ctx := context.Background()
	if err := mux.Apply(ctx, []proxy.Route{
		{Access: open, Hostname: "old.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Whole-config, not a delta: a withdrawn route must stop answering, which
	// is the property that lets the database stay the single source of truth.
	if err := mux.Apply(ctx, []proxy.Route{
		{Access: open, Hostname: "new.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply (replacement): %v", err)
	}

	// Withdrawn, so it is a dead end inside the preview zone -- not the
	// fallback, which would publish the panel unauthenticated.
	withdrawn := get(t, mux, "old.ondabberz.link", "/")
	if withdrawn.StatusCode != http.StatusNotFound {
		t.Fatalf("a withdrawn route returned %d, want 404", withdrawn.StatusCode)
	}
	if got := body(t, withdrawn); strings.Contains(got, "reached") {
		t.Fatalf("a withdrawn route still answered: %q", got)
	}
	if got := body(t, get(t, mux, "new.ondabberz.link", "/")); got != "reached" {
		t.Fatalf("the new route did not answer: %q", got)
	}
}

func TestExplainsAnUpstreamThatIsNotListeningYet(t *testing.T) {
	mux := New("ondabberz.link", nil, nil)
	// Port 1 on loopback: a route that exists, pointing at nothing. This is
	// the ordinary state while a VM boots and dependencies install.
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: "127.0.0.1", UpstreamPort: 1},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	resp := get(t, mux, "web-shop-cart.ondabberz.link", "/")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	text := body(t, resp)
	// A bare 502 reads as "the preview is broken" during the minutes when it
	// is merely not up yet, so the page has to say which it is.
	if !strings.Contains(text, "not answering yet") || !strings.Contains(text, "normal while the machine boots") {
		t.Fatalf("the 502 page does not explain itself: %q", text)
	}
	if !strings.Contains(text, "web-shop-cart.ondabberz.link") {
		t.Fatalf("the 502 page does not name the hostname: %q", text)
	}
}

func TestApplyRefusesDuplicateHostnames(t *testing.T) {
	mux := New("ondabberz.link", nil, nil)
	err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "same.ondabberz.link", UpstreamHost: "10.0.0.1", UpstreamPort: 3000},
		{Access: open, Hostname: "same.ondabberz.link", UpstreamHost: "10.0.0.2", UpstreamPort: 3000},
	})
	if err == nil {
		t.Fatal("two forks were allowed to claim one hostname; which preview you reach would be a coin flip")
	}
}

func TestRequestBeforeTheFirstPublishIsNotAFailure(t *testing.T) {
	mux := New("ondabberz.link", nil, nil)
	// No Apply and no fallback: the window during startup. A hostname outside
	// the preview zone must say the control plane is not ready rather than
	// report a missing page.
	resp := get(t, mux, "dabberz.dab.im", "/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while still wiring", resp.StatusCode)
	}
}

func TestStreamingResponsesAreFlushedThrough(t *testing.T) {
	// Hot reload and server-sent events are long-lived streams. If the proxy
	// buffers, a live preview arrives in batches or not at all.
	host, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream recorder cannot flush")
			return
		}
		io.WriteString(w, "first\n")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, "second\n")
		flusher.Flush()
	}))

	mux := New("ondabberz.link", nil, nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "web.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := body(t, get(t, mux, "web.ondabberz.link", "/events")); got != "first\nsecond\n" {
		t.Fatalf("stream did not arrive intact: %q", got)
	}
}

func TestRoutesAreReportedForDiagnostics(t *testing.T) {
	mux := New("ondabberz.link", nil, nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: "web.ondabberz.link", UpstreamHost: "172.30.0.2", UpstreamPort: 41000},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	routes := mux.Routes()
	if routes["web.ondabberz.link"] != "172.30.0.2:41000" {
		t.Fatalf("Routes() = %v", routes)
	}
}

// TestAVisitorCannotClaimToBeSomeone is the one that matters most here.
//
// An app behind the platform is told to trust the identity headers it
// receives, which is only safe because nothing else can set them. A visitor
// who could would have the admin surface of somebody else's installation, and
// nothing about it would look broken.
func TestAVisitorCannotClaimToBeSomeone(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Signed in as somebody real, so that the forged headers are competing
	// with an assertion rather than with silence. Getting this order wrong --
	// asserting first and stripping after, or merging the two -- is how a
	// forged header survives, so the test puts them in conflict.
	who := &stubVisitors{as: &domain.User{ID: "usr_real", Email: "real@example.com"}}
	mux := New("ondabberz.link", who, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mux.Apply(context.Background(), []proxy.Route{{
		Access:       open,
		Hostname:     "app.ondabberz.link",
		UpstreamHost: "127.0.0.1",
		UpstreamPort: upstreamPort(t, upstream.URL),
		OwnerID:      "usr_real",
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	edge := httptest.NewServer(mux)
	defer edge.Close()

	req, err := http.NewRequest(http.MethodGet, edge.URL, nil) //nolint:noctx // a loopback request in a test
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.ondabberz.link"
	// Everything a forger would try.
	req.Header.Set("X-Dabberz-User", "usr_someone_else")
	req.Header.Set("x-dabberz-email", "victim@example.com")
	req.Header.Set("X-DABBERZ-ROLE", "owner")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	arrived := <-seen
	if got := arrived.Get("X-Dabberz-User"); got != "usr_real" {
		t.Errorf("the app was told the visitor is %q, want usr_real", got)
	}
	if got := arrived.Get("X-Dabberz-Email"); got != "real@example.com" {
		t.Errorf("the app was told the email is %q, want real@example.com", got)
	}
	if got := arrived.Get("X-Dabberz-Role"); got != "" {
		t.Errorf("a forged header the platform does not assert survived: %q", got)
	}
}

// TestAnAnonymousVisitorIsAnnouncedAsNobody covers an app's public surface: a
// form being filled in by somebody who has no account and never will. The
// edge must forward the request with nothing asserted rather than refuse it,
// and the app decides which of its own paths need a visitor.
func TestAnAnonymousVisitorIsAnnouncedAsNobody(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// No credential, and a credential that is refused: both are strangers,
	// and neither may be announced as anyone.
	for name, who := range map[string]*stubVisitors{
		"no credential": {err: auth.ErrNoCredentials},
		"refused":       {err: auth.ErrUnauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			mux := New("ondabberz.link", who, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := mux.Apply(context.Background(), []proxy.Route{{
				Access:       open,
				Hostname:     "app.ondabberz.link",
				UpstreamHost: "127.0.0.1",
				UpstreamPort: upstreamPort(t, upstream.URL),
			}}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			edge := httptest.NewServer(mux)
			defer edge.Close()

			req, err := http.NewRequest(http.MethodGet, edge.URL, nil) //nolint:noctx // a loopback request in a test
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "app.ondabberz.link"

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("an anonymous visitor to a public surface got %d", resp.StatusCode)
			}

			arrived := <-seen
			for header := range arrived {
				if strings.HasPrefix(strings.ToLower(header), "x-dabberz-") {
					t.Errorf("a stranger was announced as somebody: %s: %q", header, arrived.Get(header))
				}
			}
		})
	}
}

// TestASignedInStrangerIsNotAnnouncedOnSomebodyElsesHostname is the
// cross-tenant case, and the reason a route carries its owner.
//
// Every customer of the platform has a dabberz account, so "is signed in" is
// nearly everyone. An app behind the edge treats an asserted identity as
// permission to administer the installation -- it has no way to check
// ownership itself, and should not have to -- so an edge that announced any
// signed-in visitor would make each customer an administrator of every other
// customer's installation, with nothing looking broken.
func TestASignedInStrangerIsNotAnnouncedOnSomebodyElsesHostname(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	who := &stubVisitors{as: &domain.User{ID: "usr_stranger", Email: "stranger@example.com"}}
	mux := New("ondabberz.link", who, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mux.Apply(context.Background(), []proxy.Route{{
		Access:       open,
		Hostname:     "app.ondabberz.link",
		UpstreamHost: "127.0.0.1",
		UpstreamPort: upstreamPort(t, upstream.URL),
		OwnerID:      "usr_somebody_else",
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	edge := httptest.NewServer(mux)
	defer edge.Close()

	req, err := http.NewRequest(http.MethodGet, edge.URL, nil) //nolint:noctx // a loopback request in a test
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.ondabberz.link"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	arrived := <-seen
	for header := range arrived {
		if strings.HasPrefix(strings.ToLower(header), "x-dabberz-") {
			t.Errorf("a signed-in stranger was announced on somebody else's installation: %s: %q",
				header, arrived.Get(header))
		}
	}
}

// TestARouteNobodyOwnsAnnouncesNobody covers a preview URL that outlived the
// workspace it pointed at: the owner is unknown, so there is nobody whose
// identity may be asserted, even for a visitor who is signed in.
func TestARouteNobodyOwnsAnnouncesNobody(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	who := &stubVisitors{as: &domain.User{ID: "usr_real", Email: "real@example.com"}}
	mux := New("ondabberz.link", who, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := mux.Apply(context.Background(), []proxy.Route{{
		Access:       open,
		Hostname:     "orphan.ondabberz.link",
		UpstreamHost: "127.0.0.1",
		UpstreamPort: upstreamPort(t, upstream.URL),
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	edge := httptest.NewServer(mux)
	defer edge.Close()

	req, err := http.NewRequest(http.MethodGet, edge.URL, nil) //nolint:noctx // a loopback request in a test
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "orphan.ondabberz.link"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	arrived := <-seen
	for header := range arrived {
		if strings.HasPrefix(strings.ToLower(header), "x-dabberz-") {
			t.Errorf("a route with no owner announced somebody: %s: %q", header, arrived.Get(header))
		}
	}
}

// stubVisitors is the edge's identity source under test.
type stubVisitors struct {
	as  *domain.User
	err error
}

func (s *stubVisitors) Visitor(context.Context, *http.Request) (*domain.User, error) {
	return s.as, s.err
}

// upstreamPort reads the port out of an httptest server's URL.
func upstreamPort(t *testing.T, raw string) int {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}
