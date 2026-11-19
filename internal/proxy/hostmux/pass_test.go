package hostmux

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/previewpass"
	"github.com/dabbers/devex/internal/proxy"
)

// passEdge is an edge for one preview owned by usr_owner, accepting passes
// from issuer, with nobody signed in by any other means.
func passEdge(t *testing.T, issuer *previewpass.Issuer) (*httptest.Server, chan http.Header) {
	t.Helper()
	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	mux := New("ondabberz.link", &stubVisitors{err: auth.ErrNoCredentials},
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithPasses(issuer)
	if err := mux.Apply(context.Background(), []proxy.Route{{
		Access:       open,
		Hostname:     "app-change.ondabberz.link",
		UpstreamHost: "127.0.0.1",
		UpstreamPort: upstreamPort(t, upstream.URL),
		OwnerID:      "usr_owner",
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	edge := httptest.NewServer(mux)
	t.Cleanup(edge.Close)
	return edge, seen
}

func visitWithPass(t *testing.T, edge *httptest.Server, pass string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, edge.URL, nil) //nolint:noctx // a loopback request in a test
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app-change.ondabberz.link"
	req.Header.Add("Cookie", "app_session=keep-me")
	req.AddCookie(&http.Cookie{Name: previewpass.CookieName, Value: pass})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
}

// TestAPassLetsTheCheckSeeThePreviewAsItsOwner: the verifier has nobody's
// session, so without this a page behind sign-in is a 401 to it, and on
// dbz-alpha that meant no Forms change could ever be promoted.
func TestAPassLetsTheCheckSeeThePreviewAsItsOwner(t *testing.T) {
	t.Parallel()
	issuer, err := previewpass.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	edge, seen := passEdge(t, issuer)

	pass, err := issuer.Mint("app-change.ondabberz.link",
		&domain.User{ID: "usr_owner", Email: "owner@example.com"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	visitWithPass(t, edge, pass)

	arrived := <-seen
	if got := arrived.Get(proxy.UserHeader); got != "usr_owner" {
		t.Fatalf("the owner's pass was not announced as the owner: %s = %q", proxy.UserHeader, got)
	}
	// The app is code an agent may just have written. It gets who is asking,
	// never the thing that proved it.
	cookies := arrived.Get("Cookie")
	if strings.Contains(cookies, previewpass.CookieName) {
		t.Fatalf("the pass reached the app: Cookie: %q", cookies)
	}
	if !strings.Contains(cookies, "app_session=keep-me") {
		t.Fatalf("the app's own cookies were dropped with the pass: Cookie: %q", cookies)
	}
}

func TestAPassThatIsNotForThisPreviewIsNobody(t *testing.T) {
	t.Parallel()
	issuer, err := previewpass.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := previewpass.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	owner := &domain.User{ID: "usr_owner"}

	elsewhere, _ := issuer.Mint("app.ondabberz.link", owner, time.Minute)
	forged, _ := stranger.Mint("app-change.ondabberz.link", owner, time.Minute)
	// A genuine pass for this host, naming somebody who does not own it,
	// still asserts nothing: the owner rule is the edge's, not the pass's.
	notTheirs, _ := issuer.Mint("app-change.ondabberz.link", &domain.User{ID: "usr_other"}, time.Minute)

	for name, pass := range map[string]string{
		"for another preview": elsewhere,
		"from another daemon": forged,
		"for a non-owner":     notTheirs,
		"not a pass at all":   "garbage",
	} {
		t.Run(name, func(t *testing.T) {
			edge, seen := passEdge(t, issuer)
			visitWithPass(t, edge, pass)
			arrived := <-seen
			for header := range arrived {
				if strings.HasPrefix(strings.ToLower(header), "x-dabberz-") {
					t.Errorf("announced as somebody: %s: %q", header, arrived.Get(header))
				}
			}
			if strings.Contains(arrived.Get("Cookie"), previewpass.CookieName) {
				t.Error("a rejected pass still reached the app")
			}
		})
	}
}
