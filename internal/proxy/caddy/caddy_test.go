package caddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/proxy"
)

func testRoutes() []proxy.Route {
	return []proxy.Route{
		{Hostname: "preview-web-photos.dab.im", UpstreamHost: "172.30.0.6", UpstreamPort: 5173},
		{Hostname: "preview-web-ratings.dab.im", UpstreamHost: "172.30.0.2", UpstreamPort: 5173},
	}
}

func testConfig() Config {
	return Config{
		WildcardDomain: "dab.im",
		ACMEEmail:      "ops@dab.im",
		DNSProvider:    map[string]any{"name": "cloudflare", "api_token": "{env.CF_API_TOKEN}"},
	}
}

func TestBuildConfigRoutesEachHostnameToItsFork(t *testing.T) {
	cfg, err := New(testConfig(), nil).BuildConfig(testRoutes())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	server, ok := cfg.Apps.HTTP.Servers["dabberz"]
	if !ok {
		t.Fatalf("no dabberz server in config: %+v", cfg.Apps.HTTP.Servers)
	}
	if len(server.Routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(server.Routes))
	}
	// Routes come back sorted, so this mapping is deterministic.
	first := server.Routes[0]
	if first.Match[0].Host[0] != "preview-web-photos.dab.im" {
		t.Fatalf("first route host = %q", first.Match[0].Host[0])
	}
	if first.Handle[0].Upstreams[0].Dial != "172.30.0.6:5173" {
		t.Fatalf("first route upstream = %q", first.Handle[0].Upstreams[0].Dial)
	}
	if !first.Terminal {
		t.Error("preview routes should be terminal so nothing masks a live preview")
	}
}

func TestBuildConfigRequestsAWildcardCertificate(t *testing.T) {
	cfg, err := New(testConfig(), nil).BuildConfig(testRoutes())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if cfg.Apps.TLS == nil || cfg.Apps.TLS.Automation == nil || len(cfg.Apps.TLS.Automation.Policies) != 1 {
		t.Fatalf("no TLS automation policy generated: %+v", cfg.Apps.TLS)
	}
	policy := cfg.Apps.TLS.Automation.Policies[0]
	var sawWildcard bool
	for _, subject := range policy.Subjects {
		if subject == "*.dab.im" {
			sawWildcard = true
		}
	}
	if !sawWildcard {
		t.Fatalf("subjects = %v, want the wildcard so new forks need no issuance", policy.Subjects)
	}
	// Only DNS-01 can issue a wildcard, so the challenge config must be present.
	if policy.Issuers[0].Challenges == nil || policy.Issuers[0].Challenges.DNS == nil {
		t.Fatal("wildcard issuance requires a DNS-01 challenge configuration")
	}
	if policy.Issuers[0].Challenges.DNS.Provider["name"] != "cloudflare" {
		t.Fatalf("DNS provider config was not passed through: %+v", policy.Issuers[0].Challenges.DNS.Provider)
	}
}

func TestBuildConfigOmitsTLSWithoutADomain(t *testing.T) {
	cfg, err := New(Config{}, nil).BuildConfig(testRoutes())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if cfg.Apps.TLS != nil {
		t.Fatal("no wildcard domain configured, so no TLS automation should be emitted")
	}
}

func TestBuildConfigAppendsFallbackLast(t *testing.T) {
	cfg := testConfig()
	cfg.FallbackUpstream = "127.0.0.1:8080"
	doc, err := New(cfg, nil).BuildConfig(testRoutes())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	routes := doc.Apps.HTTP.Servers["dabberz"].Routes
	if len(routes) != 3 {
		t.Fatalf("got %d routes, want 2 previews plus a fallback", len(routes))
	}
	// The catch-all must come last, or it would swallow live previews.
	last := routes[len(routes)-1]
	if last.Match[0].Host[0] != "*.dab.im" {
		t.Fatalf("last route host = %q, want the wildcard fallback", last.Match[0].Host[0])
	}
	if last.Terminal {
		t.Error("the fallback route should not be marked terminal")
	}
}

func TestBuildConfigRejectsConflictingRoutes(t *testing.T) {
	_, err := New(testConfig(), nil).BuildConfig([]proxy.Route{
		{Hostname: "preview-a.dab.im", UpstreamHost: "10.0.0.1", UpstreamPort: 1},
		{Hostname: "preview-a.dab.im", UpstreamHost: "10.0.0.2", UpstreamPort: 2},
	})
	if err == nil {
		t.Fatal("expected conflicting routes to be rejected before reaching Caddy")
	}
}

func TestApplyPostsToTheAdminAPI(t *testing.T) {
	var got struct {
		path string
		body []byte
		ct   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.ct = r.Header.Get("Content-Type")
		got.body, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.AdminEndpoint = srv.URL
	if err := New(cfg, srv.Client()).Apply(context.Background(), testRoutes()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got.path != "/load" {
		t.Fatalf("posted to %q, want /load", got.path)
	}
	if got.ct != "application/json" {
		t.Fatalf("content type = %q", got.ct)
	}
	var decoded Document
	if err := json.Unmarshal(got.body, &decoded); err != nil {
		t.Fatalf("Caddy would not be able to parse the body: %v", err)
	}
	if len(decoded.Apps.HTTP.Servers["dabberz"].Routes) != 2 {
		t.Fatalf("posted config carried %d routes", len(decoded.Apps.HTTP.Servers["dabberz"].Routes))
	}
}

func TestApplySurfacesCaddysRejectionReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		//nolint:errcheck // test server
		w.Write([]byte(`{"error":"unknown module: reverse_proxyy"}`))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.AdminEndpoint = srv.URL
	err := New(cfg, srv.Client()).Apply(context.Background(), testRoutes())
	if err == nil {
		t.Fatal("expected a rejected config to return an error")
	}
	// The operator needs Caddy's own explanation, not just a status code.
	if !strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("error lost Caddy's explanation: %v", err)
	}
}

func TestApplyReportsTransportFailures(t *testing.T) {
	cfg := testConfig()
	cfg.AdminEndpoint = "http://127.0.0.1:1" // nothing listening
	if err := New(cfg, nil).Apply(context.Background(), testRoutes()); err == nil {
		t.Fatal("expected an unreachable admin endpoint to fail")
	}
}

func TestFileWriterInstallsConfigAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "caddy.json")
	w := NewFileWriter(testConfig(), path)

	if err := w.Apply(context.Background(), testRoutes()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var decoded Document
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("written config is not valid JSON: %v", err)
	}
	if len(decoded.Apps.HTTP.Servers["dabberz"].Routes) != 2 {
		t.Fatal("written config does not carry the routes")
	}
	// No temporary file should be left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file was left behind (err = %v)", err)
	}

	// Rewriting replaces the previous config wholesale.
	if err := w.Apply(context.Background(), testRoutes()[:1]); err != nil {
		t.Fatalf("Apply (rewrite): %v", err)
	}
	raw, _ = os.ReadFile(path)
	//nolint:errcheck // already validated above
	json.Unmarshal(raw, &decoded)
	if len(decoded.Apps.HTTP.Servers["dabberz"].Routes) != 1 {
		t.Fatal("rewriting did not replace the previous route set")
	}
}

func TestRouterNames(t *testing.T) {
	if New(testConfig(), nil).Name() != "caddy" {
		t.Error("unexpected client name")
	}
	if NewFileWriter(testConfig(), "/tmp/x.json").Name() != "caddy-file" {
		t.Error("unexpected file writer name")
	}
}
