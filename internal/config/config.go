// Package config loads the dabberz control plane's configuration.
//
// Configuration is a single YAML file plus environment overrides for the
// handful of values that are secrets. Secrets are never written into the file
// by dabberz itself: the file is meant to be readable and checked in, with
// credentials supplied by the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/merge"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/proxy/caddy"
	"github.com/dabbers/devex/internal/tripwire"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/firecracker"
)

// Environment variables holding credentials. These override the file so that
// nothing secret has to live in it.
const (
	EnvMasterKey    = "DABBERZ_MASTER_KEY"
	EnvOrchestrator = "DABBERZ_ORCHESTRATOR_API_KEY"
	// EnvDataAdminDSN reaches the Postgres cluster installations are
	// provisioned on. A credential, so it belongs in the environment rather
	// than in a file somebody might check in.
	EnvDataAdminDSN = "DABBERZ_DATABASE_ADMIN_DSN"
	// EnvStorageSecret is the object store's secret key.
	EnvStorageSecret = "DABBERZ_STORAGE_SECRET_KEY"
	EnvClaudeToken   = "CLAUDE_CODE_OAUTH_TOKEN"
	// Clerk's publishable key is not a credential -- it is sent to the browser
	// -- but it is read from the environment alongside the secret so a
	// deployment configures both in one place.
	EnvClerkPublishable = "CLERK_PUBLISHABLE_KEY"
	EnvClerkSecret      = "CLERK_SECRET_KEY"
)

// Config is the whole control-plane configuration.
type Config struct {
	// Owner is the single user everything is scoped to in v1.
	Owner string `yaml:"owner"`
	// DataDir holds the database, VM workspaces and the memory store.
	DataDir string `yaml:"data_dir"`
	// MasterKey is the hex-encoded secrets key. Prefer the environment.
	MasterKey string `yaml:"master_key"`

	Auth      AuthConfig          `yaml:"auth"`
	Catalog   CatalogConfig       `yaml:"catalog"`
	Blocks    BlocksConfig        `yaml:"blocks"`
	Data      DataConfig          `yaml:"data"`
	Server    ServerConfig        `yaml:"server"`
	Driver    DriverConfig        `yaml:"driver"`
	Scheduler SchedulerConfig     `yaml:"scheduler"`
	Preview   preview.Config      `yaml:"preview"`
	Caddy     caddy.Config        `yaml:"caddy"`
	LLM       llm.Config          `yaml:"llm"`
	Agent     agent.Config        `yaml:"agent"`
	Verify    verify.Config       `yaml:"verify"`
	Merge     merge.Config        `yaml:"merge"`
	Tripwire  tripwire.Thresholds `yaml:"tripwire"`
}

// CatalogConfig is where installable apps come from.
type CatalogConfig struct {
	// Dir holds one directory per app, each with a dabberz.yaml. Empty means
	// this deployment installs nothing, which is a working configuration for
	// a control plane that only drives workspaces.
	Dir string `yaml:"dir"`
}

// BlocksConfig is what this deployment offers an installed app, apart from
// the database, which is provisioned per installation.
//
// An operator asserting what exists. An app whose manifest declares something
// not listed here does not start, and says which -- because an app started
// without the mail it declared does not fail at boot, it fails when the first
// notification does not arrive, and by then nobody connects the two.
type BlocksConfig struct {
	// MailURL is an SMTP endpoint the platform owns, along with its sending
	// domain and reputation.
	MailURL string `yaml:"mail_url"`
	// Storage is an S3-compatible bucket. Credentials belong in the
	// environment rather than here.
	Storage StorageConfig `yaml:"storage"`
	// RealtimeURL is the shared sync fabric.
	RealtimeURL string `yaml:"realtime_url"`
	// SearchURL is the shared index.
	SearchURL string `yaml:"search_url"`
}

// StorageConfig points at an object store.
type StorageConfig struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"-"`
}

// DataConfig is the data block: the Postgres cluster installations are
// provisioned on.
//
// Separate from the control plane's own store, which is SQLite. One process on
// one machine can run on SQLite; somebody's live business cannot.
type DataConfig struct {
	// AdminDSN reaches the cluster as a role that may create databases and
	// roles. Prefer the environment: DABBERZ_DATABASE_ADMIN_DSN.
	AdminDSN string `yaml:"admin_dsn"`
	// PublicHost is what apps are told to connect to, when that differs from
	// the address the control plane administers through.
	PublicHost string `yaml:"public_host"`
	// SnapshotDir holds database snapshots. It must be on storage that
	// outlives the machine: a snapshot on the same disk as the database it
	// protects is a copy, not a backup.
	SnapshotDir string `yaml:"snapshot_dir"`
}

// ServerConfig configures the HTTP control plane.
type ServerConfig struct {
	// Addr is the listen address for the web API.
	Addr string `yaml:"addr"`
	// ReadTimeout and WriteTimeout bound a request. The write timeout has to
	// accommodate the event stream, which is long-lived by design.
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

// DriverConfig selects and configures the VM driver.
type DriverConfig struct {
	// Kind is "firecracker" or "local".
	Kind string `yaml:"kind"`
	// ForkResources is the allocation each fork VM receives.
	ForkResources vm.Resources `yaml:"fork_resources"`
	// Image is the golden image forks boot.
	Image string `yaml:"image"`
	// Local configures the development driver.
	Local LocalDriverConfig `yaml:"local"`
	// Firecracker configures the production driver.
	Firecracker firecracker.Config `yaml:"firecracker"`
}

// LocalDriverConfig configures the development driver.
type LocalDriverConfig struct {
	// Total is the capacity the local driver pretends to have.
	Total        vm.Resources `yaml:"total"`
	MaxInstances int          `yaml:"max_instances"`
}

// SchedulerConfig configures fork admission.
type SchedulerConfig struct {
	Interval time.Duration `yaml:"interval"`
	// HeadOfLineBlocking keeps the queue strictly first-come-first-served.
	HeadOfLineBlocking bool `yaml:"head_of_line_blocking"`
}

// Default returns a configuration suitable for local development.
func Default() Config {
	return Config{
		Owner:   "owner@example.com",
		DataDir: "var",
		Server: ServerConfig{
			Addr:        "127.0.0.1:8080",
			ReadTimeout: 30 * time.Second,
			// Long, because the activity stream is a long-lived response.
			WriteTimeout: 0,
		},
		Driver: DriverConfig{
			Kind:          "local",
			ForkResources: vm.Resources{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 20},
			Image:         "dabberz-golden",
			Local: LocalDriverConfig{
				Total: vm.Resources{VCPUs: 8, MemoryMiB: 16384, DiskGiB: 200},
			},
		},
		Scheduler: SchedulerConfig{Interval: 5 * time.Second},
		Preview:   preview.Config{Domain: "dab.im", Scheme: "https"},
		LLM:       llm.Config{BaseURL: llm.DefaultBaseURL, Model: llm.DefaultModel},
		Verify: verify.Config{
			Profiles: verify.DefaultProfiles,
			// Verification is required for anything to merge, so the machine
			// that performs it is provisioned by default rather than being a
			// step an operator has to discover.
			VM: verify.VMConfig{AutoProvision: true},
		},
		Merge:    merge.Config{Remote: "origin", Push: true},
		Tripwire: tripwire.Defaults(),
	}
}

// Load reads a configuration file, applies defaults for anything it omits, and
// overlays credentials from the environment.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		// Decoding over the defaults leaves unset keys at their default.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv overlays credentials from the environment, which always win over
// the file.
func (c *Config) applyEnv() {
	if v := os.Getenv(EnvMasterKey); v != "" {
		c.MasterKey = v
	}
	if v := os.Getenv(EnvOrchestrator); v != "" {
		c.LLM.APIKey = v
	}
	if v := os.Getenv(EnvClaudeToken); v != "" {
		c.Agent.OAuthToken = v
	}
	if v := os.Getenv(EnvClerkPublishable); v != "" {
		c.Auth.Clerk.PublishableKey = v
	}
	if v := os.Getenv(EnvDataAdminDSN); v != "" {
		c.Data.AdminDSN = v
	}
	if v := os.Getenv(EnvStorageSecret); v != "" {
		c.Blocks.Storage.SecretKey = v
	}
	if v := os.Getenv(EnvClerkSecret); v != "" {
		c.Auth.Clerk.SecretKey = v
	}
}

// Env reads a value from the process environment. It exists so configuration
// assembly stays in this package rather than spreading os.Getenv through the
// components.
func (c *Config) Env(name string) string { return os.Getenv(name) }

// loopbackAddr reports whether a listen address is reachable only from this
// machine. A missing host means every interface, which is the dangerous
// reading, so it is not loopback.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "*", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate checks the configuration is usable before anything starts.
func (c *Config) Validate() error {
	var problems []string

	if c.Owner == "" {
		problems = append(problems, "owner is required")
	}
	if c.DataDir == "" {
		problems = append(problems, "data_dir is required")
	}
	if c.Server.Addr == "" {
		problems = append(problems, "server.addr is required")
	}

	// Refused rather than warned about. Single-user auth accepts anything, so
	// on an address the internet can reach it hands a shell, the secrets vault
	// and the audit trail to whoever connects -- and it does that while
	// looking like a working deployment, which is the part that makes a
	// warning the wrong instrument. There are two ways out and both are
	// configuration, so neither needs a flag to override this.
	if c.Auth.Kind == AuthSingle && c.Server.Addr != "" && !loopbackAddr(c.Server.Addr) {
		problems = append(problems, fmt.Sprintf(
			"auth.kind is single and server.addr is %s, which is not loopback: "+
				"every request would be treated as %s without being asked for anything. "+
				"Bind to 127.0.0.1 and reach it through a tunnel, or configure an identity provider",
			c.Server.Addr, c.Owner))
	}

	switch c.Driver.Kind {
	case "local", "":
		c.Driver.Kind = "local"
	case "firecracker":
		if err := c.Driver.Firecracker.Validate(); err != nil {
			problems = append(problems, err.Error())
		}
	default:
		problems = append(problems, fmt.Sprintf("driver.kind %q is not one of local, firecracker", c.Driver.Kind))
	}

	if err := c.Preview.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if err := c.Auth.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if c.MasterKey == "" {
		problems = append(problems, "a secrets master key is required; set "+EnvMasterKey+" (generate one with `dabberzctl keygen`)")
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Warnings reports configuration that is valid but will limit what dabberz can
// do, so the daemon can say so at startup rather than failing later.
func (c *Config) Warnings() []string {
	var warnings []string

	if c.LLM.APIKey == "" {
		warnings = append(warnings, "no orchestrator API key ("+EnvOrchestrator+"); planning and monorepo discovery are unavailable")
	}
	if c.Agent.OAuthToken == "" {
		warnings = append(warnings, "no Claude Code token ("+EnvClaudeToken+"); coding agents will not authenticate")
	}
	if c.Verify.UIInstanceID == "" && !c.Verify.VM.AutoProvision {
		warnings = append(warnings, "no shared UI VM (verify.ui_instance_id is unset and verify.vm.auto_provision is off); verification cannot run, so no fork can reach the merge gate")
	}
	if c.Driver.Kind == "local" {
		warnings = append(warnings, "using the local VM driver: agents run on the host with no isolation, which is for development only")
	}
	if c.Auth.Kind == AuthSingle {
		warnings = append(warnings,
			"auth.kind is single: every request is treated as "+c.Owner+" without being asked for anything, "+
				"so whoever can reach this port has a shell, the secrets vault and the whole audit trail. "+
				"Bind to loopback, or configure a provider")
	}
	switch c.PreviewRouter() {
	case preview.RouterNone:
		warnings = append(warnings,
			"preview.router is none; preview hostnames are allocated and shown but nothing serves them")
	case preview.RouterInProcess:
		warnings = append(warnings,
			"serving previews in-process: correct behind a CDN that terminates TLS, but this machine "+
				"answers preview hostnames over plain HTTP, so do not expose it directly")
	}
	return warnings
}

// PreviewRouter resolves which router will actually be used, applying the
// inference that preview.Router leaves to the caller.
//
// Caddy is only chosen when it is configured, because configuring the
// in-process router wrongly serves previews over plain HTTP while pointing at
// an admin API that is not there fails on every allocation instead.
func (c *Config) PreviewRouter() preview.Router {
	if c.Preview.Router != preview.RouterAuto {
		return c.Preview.Router
	}
	if c.Caddy.WildcardDomain != "" {
		return preview.RouterCaddy
	}
	return preview.RouterInProcess
}

// ErrNoConfig reports a missing configuration file.
var ErrNoConfig = errors.New("config: no configuration file")
