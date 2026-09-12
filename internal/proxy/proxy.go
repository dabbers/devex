// Package proxy models the single reverse proxy that fronts every fork.
//
// One proxy instance routes requests for the whole machine, so that a fork's
// preview is reached over the network exactly as an external user would reach
// it. The verifier deliberately takes the same path rather than talking to a
// fork's dev server directly: a co-located shortcut would not exercise TLS,
// routing or host matching, and so would not prove the preview actually works.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Route publishes one hostname to one upstream.
type Route struct {
	// Hostname is the public name, such as preview-web-ratings.dab.im.
	Hostname string `json:"hostname"`
	// UpstreamHost is the address the proxy dials: a guest's address on the
	// host bridge.
	UpstreamHost string `json:"upstream_host"`
	// UpstreamPort is the port the fork's dev server listens on.
	UpstreamPort int `json:"upstream_port"`
}

// Upstream renders the dial target.
func (r Route) Upstream() string { return fmt.Sprintf("%s:%d", r.UpstreamHost, r.UpstreamPort) }

// Validate checks a route is publishable.
func (r Route) Validate() error {
	if r.Hostname == "" {
		return errors.New("proxy: route requires a hostname")
	}
	if strings.ContainsAny(r.Hostname, " /\\") {
		return fmt.Errorf("proxy: hostname %q contains invalid characters", r.Hostname)
	}
	if r.UpstreamHost == "" {
		return fmt.Errorf("proxy: route %s requires an upstream host", r.Hostname)
	}
	if r.UpstreamPort <= 0 || r.UpstreamPort > 65535 {
		return fmt.Errorf("proxy: route %s has an invalid upstream port %d", r.Hostname, r.UpstreamPort)
	}
	return nil
}

// Router publishes the complete set of preview routes.
//
// Apply is deliberately whole-config rather than incremental: the control
// plane's database is the source of truth, so regenerating from it makes the
// proxy converge on the right state even after a crash, a manual edit or a
// restart, with no diffing to get wrong.
type Router interface {
	// Name identifies the router in logs.
	Name() string
	// Apply replaces the published routes with exactly this set.
	Apply(ctx context.Context, routes []Route) error
}

// Normalize validates a route set and returns it in a stable order, rejecting
// duplicate hostnames. Two forks answering on one hostname would make which
// preview a user reaches a coin flip, so it is caught before publishing.
func Normalize(routes []Route) ([]Route, error) {
	seen := make(map[string]string, len(routes))
	out := make([]Route, 0, len(routes))
	for _, route := range routes {
		if err := route.Validate(); err != nil {
			return nil, err
		}
		host := strings.ToLower(route.Hostname)
		if prev, dup := seen[host]; dup {
			return nil, fmt.Errorf("proxy: hostname %s is claimed by both %s and %s", host, prev, route.Upstream())
		}
		seen[host] = route.Upstream()
		route.Hostname = host
		out = append(out, route)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// Discard is a Router that publishes nothing. It lets the control plane run on
// a machine with no proxy in front of it, such as CI.
type Discard struct{}

// Name implements Router.
func (Discard) Name() string { return "discard" }

// Apply implements Router.
func (Discard) Apply(context.Context, []Route) error { return nil }

var _ Router = Discard{}
