package hostmux

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/proxy"
)

// TestOneInstallationCannotSetCookiesOnAnother: every preview is a sibling
// under one registrable domain, so a cookie an app scopes to that domain
// reaches every other customer's installation. The browser here is a real
// cookie jar with no public suffix list, which is how a browser treats the
// preview domain until it is listed.
func TestOneInstallationCannotSetCookiesOnAnother(t *testing.T) {
	const (
		theirs = "web-forms-mallory.ondabberz.link"
		yours  = "web-forms-intake.ondabberz.link"
	)

	malloryHost, malloryPort := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range []string{
			"session=chosen-by-mallory; Domain=ondabberz.link; Path=/; HttpOnly",
			"tracker=1; domain=.ondabberz.link; Path=/",
			"own=kept; Path=/; Max-Age=600; HttpOnly; SameSite=Lax",
		} {
			w.Header().Add("Set-Cookie", c)
		}
		io.WriteString(w, strings.Join(r.Header.Values("Cookie"), "; "))
	}))
	var yoursSaw []string
	yoursHost, yoursPort := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		yoursSaw = r.Header.Values("Cookie")
	}))

	mux := New("ondabberz.link", nil, nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Access: open, Hostname: theirs, UpstreamHost: malloryHost, UpstreamPort: malloryPort},
		{Access: open, Hostname: yours, UpstreamHost: yoursHost, UpstreamPort: yoursPort},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	edge := httptest.NewServer(mux)
	t.Cleanup(edge.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every hostname resolves to the edge, as it does through the CDN.
	dialer := &net.Dialer{}
	browser := &http.Client{Jar: jar, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, edge.Listener.Addr().String())
		},
	}}
	visit := func(host string) string {
		t.Helper()
		resp, err := browser.Get("http://" + host + "/")
		if err != nil {
			t.Fatalf("GET %s: %v", host, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}

	visit(theirs)
	visit(yours)
	if len(yoursSaw) != 0 {
		t.Fatalf("another installation's cookies reached this one: %q", yoursSaw)
	}

	// The app's cookies still work where they were set, Domain or not.
	back := visit(theirs)
	for _, want := range []string{"own=kept", "session=chosen-by-mallory", "tracker=1"} {
		if !strings.Contains(back, want) {
			t.Fatalf("the app lost its own cookie %q on its own hostname: got %q", want, back)
		}
	}
}

// TestKeepingCookiesToTheirHostKeepsEverythingElse: only Domain goes. An
// attribute dropped by accident -- HttpOnly, Secure, SameSite -- would weaken
// the app's cookie without anything appearing broken.
func TestKeepingCookiesToTheirHostKeepsEverythingElse(t *testing.T) {
	header := http.Header{}
	header.Add("Set-Cookie", "a=1; Domain=ondabberz.link; Path=/; Secure; HttpOnly; SameSite=Strict; Partitioned")
	header.Add("Set-Cookie", "b=x=y; Path=/app")
	hostOnlyCookies(header)

	got := header.Values("Set-Cookie")
	want := []string{"a=1; Path=/; Secure; HttpOnly; SameSite=Strict; Partitioned", "b=x=y; Path=/app"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cookie %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
