// Package hostmux serves preview routes from inside the control plane instead
// of configuring an external proxy.
//
// The architecture originally required Caddy for one specific reason: a
// wildcard certificate can only be issued over DNS-01, and that is what lets a
// new fork be reachable the moment it starts rather than after an issuance
// round trip. When a CDN fronts the preview zone it terminates TLS at the
// edge, so the origin never needs that certificate -- and with it goes the
// only thing Caddy was there to do. What remains is routing by Host header to
// an address, which is a few dozen lines in process.
//
// This is not a shortcut around the network path. The verifier and the user
// still reach a preview over its public hostname through the same CDN and the
// same host matching an outside visitor would; nothing talks to a fork's dev
// server directly. Where TLS must terminate at the origin -- no CDN, or a
// wildcard the CDN will not cover -- Caddy is still the right router, which is
// why both implement proxy.Router and the choice is configuration.
package hostmux

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/dabbers/devex/internal/proxy"
)

// Mux routes requests for published preview hostnames to their upstreams and
// hands everything else to a fallback handler, which is the control plane's
// own API and UI.
type Mux struct {
	mu sync.RWMutex
	// domain is the preview zone. A hostname inside it that has no route is
	// NOT handed to the fallback, which is what keeps the control plane from
	// being reachable under a preview hostname.
	domain   string
	routes   map[string]*route
	fallback http.Handler
	logger   *slog.Logger
}

type route struct {
	upstream string
	proxy    *httputil.ReverseProxy
}

// New returns an empty Mux for the given preview domain. Nothing is routed
// until Apply is called.
//
// The domain is not decoration. The panel and the previews share one
// listener but must not share an access policy: whatever sits in front
// authenticates the control plane's hostname and deliberately does not
// authenticate the preview zone. If an unmatched preview hostname fell
// through to the control plane, the panel would be reachable, unauthenticated,
// at any name under the preview domain. So inside the zone an unknown
// hostname is a dead end, and only hostnames outside it reach the fallback.
func New(domain string, logger *slog.Logger) *Mux {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mux{
		domain: strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), ".")),
		routes: map[string]*route{},
		logger: logger,
	}
}

// SetFallback installs the handler for hostnames that are not previews.
//
// It is set after construction because the control plane's handler needs the
// router to exist first: the allocator is built before the API server, and the
// API server is what serves everything that is not a preview.
func (m *Mux) SetFallback(h http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = h
}

// Name implements proxy.Router.
func (m *Mux) Name() string { return "hostmux" }

// Apply implements proxy.Router by replacing the whole routing table.
func (m *Mux) Apply(_ context.Context, routes []proxy.Route) error {
	normalized, err := proxy.Normalize(routes)
	if err != nil {
		return err
	}

	next := make(map[string]*route, len(normalized))
	for _, r := range normalized {
		upstream := r.Upstream()
		target, err := url.Parse("http://" + upstream)
		if err != nil {
			return fmt.Errorf("hostmux: route %s has an unusable upstream %q: %w", r.Hostname, upstream, err)
		}
		hostname := r.Hostname
		next[hostname] = &route{
			upstream: upstream,
			proxy: &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(target)
					// Keep the public hostname. A dev server derives absolute
					// URLs and its hot-reload websocket address from the Host
					// it was asked for, so rewriting it to the internal
					// address makes the page load and then fail to connect.
					pr.Out.Host = pr.In.Host
					pr.SetXForwarded()
				},
				// Flush every write immediately. Hot reload and server-sent
				// events are both long-lived streams, and the default
				// buffering makes a live preview arrive in batches.
				FlushInterval: -1,
				ErrorHandler:  m.upstreamUnavailable(hostname, upstream),
				ErrorLog:      slog.NewLogLogger(m.logger.Handler(), slog.LevelDebug),
			},
		}
	}

	m.mu.Lock()
	m.routes = next
	m.mu.Unlock()
	return nil
}

// ServeHTTP routes by Host header.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := normalizeHost(r.Host)

	m.mu.RLock()
	target := m.routes[host]
	fallback := m.fallback
	m.mu.RUnlock()

	if target != nil {
		target.proxy.ServeHTTP(w, r)
		return
	}
	if m.inPreviewZone(host) {
		// Never the fallback. See New: the preview zone is not
		// access-controlled, so serving the control plane here would publish
		// the panel without authentication.
		m.noSuchPreview(w, host)
		return
	}
	if fallback == nil {
		// Only reachable before the control plane finishes wiring itself.
		http.Error(w, "the control plane is still starting", http.StatusServiceUnavailable)
		return
	}
	fallback.ServeHTTP(w, r)
}

// inPreviewZone reports whether a hostname belongs to the preview domain.
func (m *Mux) inPreviewZone(host string) bool {
	if m.domain == "" {
		return false
	}
	return host == m.domain || strings.HasSuffix(host, "."+m.domain)
}

// noSuchPreview answers a preview hostname that nothing claims.
//
// Preview URLs outlive the forks that owned them -- nothing is reclaimed
// automatically -- so a stale link is expected and gets a real page rather
// than a connection error.
func (m *Mux) noSuchPreview(w http.ResponseWriter, host string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, "No preview is published at %s.\n\n"+
		"Either this link is from a sub-task whose preview has been withdrawn,\n"+
		"or the service it pointed at no longer exists.\n", host)
}

// Routes returns the published hostname-to-upstream map, for diagnostics.
func (m *Mux) Routes() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.routes))
	for hostname, r := range m.routes {
		out[hostname] = r.upstream
	}
	return out
}

// upstreamUnavailable explains why a preview did not answer.
//
// A dev server that has not started yet is the ordinary case, not an
// exception: the VM boots, the agent clones, dependencies install, and only
// then does anything listen. A bare 502 would read as "the preview is broken"
// during the minutes when it is merely not up.
func (m *Mux) upstreamUnavailable(hostname, upstream string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		m.logger.Debug("preview upstream did not answer",
			"hostname", hostname, "upstream", upstream, "error", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// 502 rather than 503: the route exists and the gateway is fine, the
		// thing behind it is not answering.
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "This preview is not answering yet.\n\n"+
			"%s is published, but nothing is listening behind it.\n"+
			"That is normal while the machine boots and the dev server starts.\n"+
			"It is a problem if it persists: check the service's logs and that\n"+
			"its start command is right.\n", hostname)
	}
}

// normalizeHost strips any port and lowercases, so a route matches however the
// client spelled the Host header.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if stripped, _, err := net.SplitHostPort(host); err == nil {
		return stripped
	}
	return host
}

var (
	_ proxy.Router = (*Mux)(nil)
	_ http.Handler = (*Mux)(nil)
)
