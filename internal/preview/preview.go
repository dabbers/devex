// Package preview allocates and publishes the live URL for each fork.
//
// Every fork gets its own hostname under a wildcard domain, pointed at that
// fork's dev server. There is no deploy step, cache to clear or refresh to
// trigger: the user and the verifier both open the same in-flight preview the
// coding agent is editing.
package preview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/proxy"
	"github.com/dabbers/devex/internal/store"
)

// Default port range for fork previews, chosen to sit above the ephemeral
// range most distributions hand out.
const (
	DefaultPortStart = 41000
	DefaultPortEnd   = 42999
)

// maxLabelLen is the DNS limit for a single hostname label.
const maxLabelLen = 63

// Config configures preview allocation.
type Config struct {
	// Domain is the wildcard domain previews live under, such as "dab.im".
	Domain string `yaml:"domain" json:"domain"`
	// Scheme is the URL scheme handed to users, "https" in any real deployment.
	Scheme string `yaml:"scheme" json:"scheme"`
	// PortStart and PortEnd bound the host ports allocated to forks.
	PortStart int `yaml:"port_start" json:"port_start"`
	PortEnd   int `yaml:"port_end" json:"port_end"`
}

func (c Config) withDefaults() Config {
	if c.Scheme == "" {
		c.Scheme = "https"
	}
	if c.PortStart == 0 {
		c.PortStart = DefaultPortStart
	}
	if c.PortEnd == 0 {
		c.PortEnd = DefaultPortEnd
	}
	return c
}

// Validate checks the configuration.
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.Domain == "" {
		return errors.New("preview: a wildcard domain is required")
	}
	if c.PortStart <= 0 || c.PortEnd > 65535 || c.PortStart > c.PortEnd {
		return fmt.Errorf("preview: port range %d-%d is invalid", c.PortStart, c.PortEnd)
	}
	return nil
}

// Allocator assigns preview hostnames and ports and publishes them to the
// reverse proxy.
type Allocator struct {
	store  *store.Store
	router proxy.Router
	cfg    Config
}

// New returns an allocator writing routes through router.
func New(st *store.Store, router proxy.Router, cfg Config) (*Allocator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if router == nil {
		router = proxy.Discard{}
	}
	return &Allocator{store: st, router: router, cfg: cfg.withDefaults()}, nil
}

// Allocate assigns a fork its hostname and host port and records the route.
// It is idempotent: a fork that already has a route keeps it, so republishing
// after a restart does not move a preview URL the user may have bookmarked.
func (a *Allocator) Allocate(ctx context.Context, fork *domain.Fork, project *domain.Project, upstreamHost string) (*store.PreviewRoute, error) {
	if upstreamHost == "" {
		return nil, fmt.Errorf("preview: fork %s has no upstream address yet", fork.ID)
	}

	if existing, err := a.store.GetPreviewRoute(ctx, fork.ID); err == nil {
		existing.UpstreamHost = upstreamHost
		if err := a.store.PutPreviewRoute(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	hostname, err := a.hostname(ctx, fork, project)
	if err != nil {
		return nil, err
	}
	port, err := a.freePort(ctx)
	if err != nil {
		return nil, err
	}

	route := &store.PreviewRoute{
		ForkID:       fork.ID,
		Hostname:     hostname,
		UpstreamHost: upstreamHost,
		UpstreamPort: port,
	}
	if err := a.store.PutPreviewRoute(ctx, route); err != nil {
		return nil, err
	}
	return route, nil
}

// URL renders the public address of a route.
func (a *Allocator) URL(route *store.PreviewRoute) string {
	return a.cfg.Scheme + "://" + route.Hostname
}

// Publish regenerates the proxy configuration from every recorded route.
//
// It always publishes the full set rather than a delta, so the proxy converges
// on the database's view even after a crash or a manual edit.
func (a *Allocator) Publish(ctx context.Context) error {
	recorded, err := a.store.ListPreviewRoutes(ctx)
	if err != nil {
		return err
	}
	routes := make([]proxy.Route, 0, len(recorded))
	for _, r := range recorded {
		routes = append(routes, proxy.Route{
			Hostname:     r.Hostname,
			UpstreamHost: r.UpstreamHost,
			UpstreamPort: r.UpstreamPort,
		})
	}
	if err := a.router.Apply(ctx, routes); err != nil {
		return fmt.Errorf("preview: publish %d routes via %s: %w", len(routes), a.router.Name(), err)
	}
	return nil
}

// Withdraw removes a fork's route and republishes. Preview URLs outlive merges
// and abandonment by design, so this only runs on explicit manual cleanup.
func (a *Allocator) Withdraw(ctx context.Context, forkID string) error {
	if err := a.store.DeletePreviewRoute(ctx, forkID); err != nil {
		return err
	}
	return a.Publish(ctx)
}

// hostname builds a unique preview hostname for a fork.
func (a *Allocator) hostname(ctx context.Context, fork *domain.Fork, project *domain.Project) (string, error) {
	projectLabel := "app"
	if project != nil {
		projectLabel = firstNonEmpty(Slug(project.Name), Slug(project.Path), "app")
	}
	forkLabel := firstNonEmpty(Slug(fork.Name), "fork")

	base := truncateLabel("preview-" + projectLabel + "-" + forkLabel)
	candidate := base + "." + a.cfg.Domain

	taken, err := a.takenHostnames(ctx, fork.ID)
	if err != nil {
		return "", err
	}
	if !taken[candidate] {
		return candidate, nil
	}

	// Two workstreams can legitimately carry the same name in different tasks,
	// so fall back to the fork id, which is unique by construction.
	suffix := "-" + shortID(fork.ID)
	candidate = truncateLabel(base, len(suffix)) + suffix + "." + a.cfg.Domain
	if taken[candidate] {
		return "", fmt.Errorf("preview: could not find a free hostname for fork %s", fork.ID)
	}
	return candidate, nil
}

// takenHostnames returns the hostnames already claimed by other forks.
func (a *Allocator) takenHostnames(ctx context.Context, exceptForkID string) (map[string]bool, error) {
	routes, err := a.store.ListPreviewRoutes(ctx)
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool, len(routes))
	for _, r := range routes {
		if r.ForkID == exceptForkID {
			continue
		}
		taken[r.Hostname] = true
	}
	return taken, nil
}

// freePort returns the lowest unallocated port in the configured range.
func (a *Allocator) freePort(ctx context.Context) (int, error) {
	used, err := a.store.UsedPreviewPorts(ctx)
	if err != nil {
		return 0, err
	}
	for port := a.cfg.PortStart; port <= a.cfg.PortEnd; port++ {
		if !used[port] {
			return port, nil
		}
	}
	// Ports are never reclaimed automatically, so exhaustion is a real
	// possibility the operator has to clear by hand.
	return 0, fmt.Errorf("preview: no free port in range %d-%d; clean up unused forks",
		a.cfg.PortStart, a.cfg.PortEnd)
}

// Slug reduces a name to a DNS label: lowercase alphanumerics and hyphens.
func Slug(in string) string {
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen
	for _, r := range strings.ToLower(in) {
		switch {
		case unicode.IsLetter(r) && r < unicode.MaxASCII, unicode.IsDigit(r) && r < unicode.MaxASCII:
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// truncateLabel clips a hostname label to the DNS limit, leaving room for an
// optional suffix, and never leaving a trailing hyphen behind.
func truncateLabel(label string, reserve ...int) string {
	limit := maxLabelLen
	for _, r := range reserve {
		limit -= r
	}
	if limit < 1 {
		limit = 1
	}
	if len(label) <= limit {
		return label
	}
	return strings.TrimRight(label[:limit], "-")
}

// shortID returns the trailing characters of an id, which carry its randomness.
func shortID(v string) string {
	_, rest, ok := strings.Cut(v, "_")
	if !ok {
		rest = v
	}
	if len(rest) <= 8 {
		return rest
	}
	return rest[len(rest)-8:]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
