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
	EnvClaudeToken  = "CLAUDE_CODE_OAUTH_TOKEN"
)

// Config is the whole control-plane configuration.
type Config struct {
	// Owner is the single user everything is scoped to in v1.
	Owner string `yaml:"owner"`
	// DataDir holds the database, VM workspaces and the memory store.
	DataDir string `yaml:"data_dir"`
	// MasterKey is the hex-encoded secrets key. Prefer the environment.
	MasterKey string `yaml:"master_key"`

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
	if c.Caddy.WildcardDomain == "" {
		warnings = append(warnings, "no Caddy wildcard domain; preview routes will not be published to a proxy")
	}
	return warnings
}

// ErrNoConfig reports a missing configuration file.
var ErrNoConfig = errors.New("config: no configuration file")
