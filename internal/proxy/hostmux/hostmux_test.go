package hostmux

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

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

	mux := New(nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "control plane")
	}))
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := body(t, get(t, mux, "web-shop-cart.ondabberz.link", "/checkout")); got != "fork dev server: /checkout" {
		t.Fatalf("preview hostname did not reach the fork: %q", got)
	}
	// The panel and the previews share a listener, so an unrouted hostname has
	// to land on the control plane rather than 404.
	if got := body(t, get(t, mux, "dabberz.dab.im", "/")); got != "control plane" {
		t.Fatalf("control-plane hostname was not served: %q", got)
	}
}

func TestPreservesThePublicHostname(t *testing.T) {
	var seen string
	host, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Host
		io.WriteString(w, "ok")
	}))

	mux := New(nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
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

	mux := New(nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "fallback")
	}))
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
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

	mux := New(nil)
	mux.SetFallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "fallback")
	}))
	ctx := context.Background()
	if err := mux.Apply(ctx, []proxy.Route{
		{Hostname: "old.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Whole-config, not a delta: a withdrawn route must stop answering, which
	// is the property that lets the database stay the single source of truth.
	if err := mux.Apply(ctx, []proxy.Route{
		{Hostname: "new.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply (replacement): %v", err)
	}

	if got := body(t, get(t, mux, "old.ondabberz.link", "/")); got != "fallback" {
		t.Fatalf("a withdrawn route still answered: %q", got)
	}
	if got := body(t, get(t, mux, "new.ondabberz.link", "/")); got != "reached" {
		t.Fatalf("the new route did not answer: %q", got)
	}
}

func TestExplainsAnUpstreamThatIsNotListeningYet(t *testing.T) {
	mux := New(nil)
	// Port 1 on loopback: a route that exists, pointing at nothing. This is
	// the ordinary state while a VM boots and dependencies install.
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "web-shop-cart.ondabberz.link", UpstreamHost: "127.0.0.1", UpstreamPort: 1},
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
	mux := New(nil)
	err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "same.ondabberz.link", UpstreamHost: "10.0.0.1", UpstreamPort: 3000},
		{Hostname: "same.ondabberz.link", UpstreamHost: "10.0.0.2", UpstreamPort: 3000},
	})
	if err == nil {
		t.Fatal("two forks were allowed to claim one hostname; which preview you reach would be a coin flip")
	}
}

func TestRequestBeforeTheFirstPublishIsNotAFailure(t *testing.T) {
	mux := New(nil)
	// No Apply and no fallback: the window during startup. It must say the
	// control plane is not ready rather than report a missing page.
	resp := get(t, mux, "anything.ondabberz.link", "/")
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

	mux := New(nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "web.ondabberz.link", UpstreamHost: host, UpstreamPort: port},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := body(t, get(t, mux, "web.ondabberz.link", "/events")); got != "first\nsecond\n" {
		t.Fatalf("stream did not arrive intact: %q", got)
	}
}

func TestRoutesAreReportedForDiagnostics(t *testing.T) {
	mux := New(nil)
	if err := mux.Apply(context.Background(), []proxy.Route{
		{Hostname: "web.ondabberz.link", UpstreamHost: "172.30.0.2", UpstreamPort: 41000},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	routes := mux.Routes()
	if routes["web.ondabberz.link"] != "172.30.0.2:41000" {
		t.Fatalf("Routes() = %v", routes)
	}
}
