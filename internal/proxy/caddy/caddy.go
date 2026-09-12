// Package caddy publishes dabberz preview routes to Caddy.
//
// This generalises the existing dab.im pattern -- one Caddy instance with
// wildcard TLS in front of every preview -- from a hand-maintained config to
// one regenerated whenever a fork appears or goes away. Routes are pushed
// through Caddy's admin API so a new preview is reachable without a restart or
// a config reload that would disturb the previews already running.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/proxy"
)

// DefaultAdminEndpoint is where Caddy listens for admin requests by default.
const DefaultAdminEndpoint = "http://127.0.0.1:2019"

// defaultTimeout bounds an admin API call.
const defaultTimeout = 15 * time.Second

// Config configures route generation.
type Config struct {
	// AdminEndpoint is Caddy's admin API base URL.
	AdminEndpoint string `yaml:"admin_endpoint" json:"admin_endpoint"`
	// ServerName names the generated HTTP server inside Caddy's config.
	ServerName string `yaml:"server_name" json:"server_name"`
	// Listen is the set of addresses the server binds.
	Listen []string `yaml:"listen" json:"listen"`
	// WildcardDomain is the domain previews live under, such as "dab.im". A
	// single wildcard certificate covers every preview, so spinning up a fork
	// never waits on certificate issuance.
	WildcardDomain string `yaml:"wildcard_domain" json:"wildcard_domain"`
	// ACMEEmail is the contact address for certificate issuance.
	ACMEEmail string `yaml:"acme_email" json:"acme_email"`
	// DNSProvider is the Caddy DNS provider module configuration used for the
	// DNS-01 challenge, which is the only challenge type that can obtain a
	// wildcard certificate. Its shape is provider-specific and passed through
	// to Caddy unchanged.
	DNSProvider map[string]any `yaml:"dns_provider" json:"dns_provider"`
	// FallbackUpstream optionally receives requests for hostnames under the
	// wildcard that match no fork, so a stale preview link gets a real page
	// from the control plane rather than a TLS-level failure.
	FallbackUpstream string `yaml:"fallback_upstream" json:"fallback_upstream"`
}

func (c Config) withDefaults() Config {
	if c.AdminEndpoint == "" {
		c.AdminEndpoint = DefaultAdminEndpoint
	}
	if c.ServerName == "" {
		c.ServerName = "dabberz"
	}
	if len(c.Listen) == 0 {
		c.Listen = []string{":443"}
	}
	return c
}

// Client publishes routes through Caddy's admin API.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a Caddy admin client.
func New(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{cfg: cfg.withDefaults(), http: httpClient}
}

// Name implements proxy.Router.
func (c *Client) Name() string { return "caddy" }

// Apply implements proxy.Router by loading a freshly generated configuration.
func (c *Client) Apply(ctx context.Context, routes []proxy.Route) error {
	cfg, err := c.BuildConfig(routes)
	if err != nil {
		return err
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("caddy: encode config: %w", err)
	}

	endpoint := strings.TrimSuffix(c.cfg.AdminEndpoint, "/") + "/load"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("caddy: build load request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("caddy: post config to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Caddy explains config rejections in the body, and that explanation is
		// the only useful part of the failure.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("caddy: config rejected with %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

// Caddy configuration structures. Only the subset dabberz generates is
// modelled; field names match Caddy's JSON config so the result loads as-is.
type (
	// Document is a complete Caddy configuration document.
	Document struct {
		Apps Apps `json:"apps"`
	}

	// Apps holds the app modules dabberz configures.
	Apps struct {
		HTTP HTTPApp `json:"http"`
		TLS  *TLSApp `json:"tls,omitempty"`
	}

	// HTTPApp holds the named servers.
	HTTPApp struct {
		Servers map[string]Server `json:"servers"`
	}

	// Server is one listening HTTP server.
	Server struct {
		Listen []string `json:"listen"`
		Routes []Route  `json:"routes"`
	}

	// Route matches requests and hands them to a chain of handlers.
	Route struct {
		Match    []Match   `json:"match,omitempty"`
		Handle   []Handler `json:"handle"`
		Terminal bool      `json:"terminal,omitempty"`
	}

	// Match selects requests by host.
	Match struct {
		Host []string `json:"host,omitempty"`
	}

	// Handler is one handler in a route's chain.
	Handler struct {
		Handler   string     `json:"handler"`
		Upstreams []Upstream `json:"upstreams,omitempty"`
	}

	// Upstream is a reverse-proxy dial target.
	Upstream struct {
		Dial string `json:"dial"`
	}

	// TLSApp carries certificate automation.
	TLSApp struct {
		Automation *Automation `json:"automation,omitempty"`
	}

	// Automation holds the issuance policies.
	Automation struct {
		Policies []Policy `json:"policies"`
	}

	// Policy is one certificate automation policy.
	Policy struct {
		Subjects []string `json:"subjects"`
		Issuers  []Issuer `json:"issuers,omitempty"`
	}

	// Issuer configures ACME issuance.
	Issuer struct {
		Module     string      `json:"module"`
		Email      string      `json:"email,omitempty"`
		Challenges *Challenges `json:"challenges,omitempty"`
	}

	// Challenges selects the ACME challenge types.
	Challenges struct {
		DNS *DNSChallenge `json:"dns,omitempty"`
	}

	// DNSChallenge configures the DNS-01 challenge.
	DNSChallenge struct {
		Provider map[string]any `json:"provider"`
	}
)

// BuildConfig renders a complete Caddy configuration for the given routes.
func (c *Client) BuildConfig(routes []proxy.Route) (*Document, error) {
	normalized, err := proxy.Normalize(routes)
	if err != nil {
		return nil, err
	}

	caddyRoutes := make([]Route, 0, len(normalized)+1)
	for _, route := range normalized {
		caddyRoutes = append(caddyRoutes, Route{
			Match:  []Match{{Host: []string{route.Hostname}}},
			Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: route.Upstream()}}}},
			// Stop at the first host match: preview hostnames are exclusive, and
			// falling through to the catch-all below would mask a live preview.
			Terminal: true,
		})
	}

	// The catch-all is registered last so it only sees hostnames no fork
	// claimed, turning a dead preview link into a real response.
	if c.cfg.FallbackUpstream != "" && c.cfg.WildcardDomain != "" {
		caddyRoutes = append(caddyRoutes, Route{
			Match:  []Match{{Host: []string{"*." + c.cfg.WildcardDomain}}},
			Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: c.cfg.FallbackUpstream}}}},
		})
	}

	cfg := &Document{
		Apps: Apps{
			HTTP: HTTPApp{
				Servers: map[string]Server{
					c.cfg.ServerName: {Listen: c.cfg.Listen, Routes: caddyRoutes},
				},
			},
		},
	}

	if c.cfg.WildcardDomain != "" {
		issuer := Issuer{Module: "acme", Email: c.cfg.ACMEEmail}
		if len(c.cfg.DNSProvider) > 0 {
			// A wildcard certificate can only be obtained over DNS-01, so the
			// provider config is what makes per-fork hostnames work without
			// issuing a certificate per preview.
			issuer.Challenges = &Challenges{DNS: &DNSChallenge{Provider: c.cfg.DNSProvider}}
		}
		cfg.Apps.TLS = &TLSApp{Automation: &Automation{Policies: []Policy{{
			Subjects: []string{"*." + c.cfg.WildcardDomain, c.cfg.WildcardDomain},
			Issuers:  []Issuer{issuer},
		}}}}
	}
	return cfg, nil
}

// FileWriter publishes routes by writing a Caddy JSON config to disk.
//
// It suits deployments that keep Caddy's config under version control or hand
// it to Caddy at startup, and it gives operators something to inspect when a
// preview is not reachable.
type FileWriter struct {
	client *Client
	path   string
}

// NewFileWriter returns a router that writes generated config to path.
func NewFileWriter(cfg Config, path string) *FileWriter {
	return &FileWriter{client: New(cfg, nil), path: path}
}

// Name implements proxy.Router.
func (w *FileWriter) Name() string { return "caddy-file" }

// Apply implements proxy.Router.
func (w *FileWriter) Apply(_ context.Context, routes []proxy.Route) error {
	cfg, err := w.client.BuildConfig(routes)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("caddy: encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o750); err != nil {
		return fmt.Errorf("caddy: create config directory: %w", err)
	}

	// Write through a temporary file so a reader never sees a half-written
	// config, and so a failed write leaves the previous one intact.
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return fmt.Errorf("caddy: write config: %w", err)
	}
	if err := os.Rename(tmp, w.path); err != nil {
		return fmt.Errorf("caddy: install config: %w", err)
	}
	return nil
}

var (
	_ proxy.Router = (*Client)(nil)
	_ proxy.Router = (*FileWriter)(nil)
)
