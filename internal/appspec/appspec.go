// Package appspec reads an app's manifest.
//
// docs/PLATFORM.md says what an app must declare to run on dabberz. This is
// the code that holds it to it. The platform reads the manifest and nothing
// else -- there is no convention-over-configuration here, because a
// convention is a thing an agent can break without anyone noticing until a
// service fails to start in production.
//
// Validation is therefore deliberately strict and says what is wrong in the
// words of the contract. A manifest that parses but means something other than
// what its author intended is worse than one that is refused.
package appspec

import (
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dabbers/devex/internal/domain"
)

// Version is the contract version this build implements. An app declares the
// version it targets; the platform supports this one and the one before it,
// which is what makes a contract change a migration rather than a flag day.
const Version = 1

// MinVersion is the oldest contract version still accepted.
const MinVersion = 1

// Filename is the manifest's name at the root of an app's repository.
const Filename = "dabberz.yaml"

// Spec is an app's manifest.
type Spec struct {
	App      string `yaml:"app"`
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Platform int    `yaml:"platform"`
	License  string `yaml:"license"`
	Upstream string `yaml:"upstream"`

	Build    Build     `yaml:"build"`
	Services []Service `yaml:"services"`
	Requires Requires  `yaml:"requires"`
	Secrets  []Secret  `yaml:"secrets"`
	MCP      MCP       `yaml:"mcp"`
	Jobs     []Job     `yaml:"jobs"`
	Extends  Extends   `yaml:"extends"`
	Checks   Checks    `yaml:"checks"`

	// Access is who the edge lets in, as the app ships. The owner can change
	// it per installation. Left out, the site is open and the app decides
	// route by route, as docs/PLATFORM.md requires of it. An app that cannot
	// do that declares internal.
	Access *domain.Access `yaml:"access"`
}

// DefaultAccess is the policy an installation starts with.
func (s *Spec) DefaultAccess() domain.Access {
	if s.Access == nil {
		return domain.Access{Mode: domain.AccessPublic}
	}
	return *s.Access
}

// Build turns a checkout into something runnable, before any service starts.
//
// Every app has one -- compiling, installing dependencies, bundling assets --
// and leaving it out meant the first app had to pretend its service command
// was also its build. It runs on the machine, from the app root, after the
// code is there and before anything is started.
type Build struct {
	Command string `yaml:"command"`
}

// Service is one long-running process the app needs.
type Service struct {
	Name       string   `yaml:"name"`
	Kind       string   `yaml:"kind"`
	Command    string   `yaml:"command"`
	Port       int      `yaml:"port"`
	HealthPath string   `yaml:"health_path"`
	Exposed    bool     `yaml:"exposed"`
	Primary    bool     `yaml:"primary"`
	DependsOn  []string `yaml:"depends_on"`
}

// Requires is what the platform must provision before the app starts.
type Requires struct {
	Database Database `yaml:"database"`
	Identity bool     `yaml:"identity"`
	Mail     bool     `yaml:"mail"`
	Storage  bool     `yaml:"storage"`
	Realtime bool     `yaml:"realtime"`
	Search   bool     `yaml:"search"`
	Payments string   `yaml:"payments"` // "", "required" or "optional"
}

// Database is the app's data store. One engine, deliberately: one backup
// story, one restore procedure, one snapshot mechanism, one migration runner.
type Database struct {
	Engine  string `yaml:"engine"`
	Version string `yaml:"version"`
}

// Secret is a credential the app needs, named and never valued. The platform
// asks the customer for it at install time rather than letting the app fail at
// boot with a stack trace nobody can read.
type Secret struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// MCP is the app's agent surface. Not optional: see PLATFORM.md.
type MCP struct {
	Path  string `yaml:"path"`
	Tools string `yaml:"tools"`
}

// Job is scheduled work. The platform invokes it; the app does not run its own
// cron, because two schedulers disagreeing is a bug nobody can reproduce.
type Job struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Path     string `yaml:"path"`
}

// Extends is where a customer's agent is allowed to work. Everything outside
// it is core: the same bytes in every installation, and upgradable in place.
type Extends struct {
	Theme        string `yaml:"theme"`
	Plugins      string `yaml:"plugins"`
	Config       string `yaml:"config"`
	SchemaPrefix string `yaml:"schema_prefix"`
}

// Checks is the app's critical-path suite. It is run from the pinned upstream
// version rather than the installation's working tree, so an agent cannot
// quietly rewrite the gate it has to pass.
type Checks struct {
	Suite string `yaml:"suite"`
}

// Load reads and validates a manifest from a directory.
func Load(dir string) (*Spec, error) {
	raw, err := os.ReadFile(path.Join(dir, Filename))
	if err != nil {
		return nil, fmt.Errorf("appspec: read %s: %w", Filename, err)
	}
	return Parse(raw)
}

// Parse reads and validates a manifest.
func Parse(raw []byte) (*Spec, error) {
	var spec Spec
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	// An unknown field is far more likely to be a typo in a field that matters
	// than a deliberate extension, and silently ignoring it means a service
	// that does not do what its manifest appears to say.
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("appspec: %s is not a readable manifest: %w", Filename, err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

var (
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)
	envPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}_$`)
)

// Validate holds a manifest to the contract.
func (s *Spec) Validate() error {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if !slugPattern.MatchString(s.App) {
		fail("app must be a lowercase slug such as %q, got %q", "forms", s.App)
	}
	if strings.TrimSpace(s.Name) == "" {
		fail("name is what a customer sees in the catalog and cannot be empty")
	}
	if strings.TrimSpace(s.Version) == "" {
		fail("version is what an installation pins and cannot be empty")
	}
	switch {
	case s.Platform == 0:
		fail("platform must declare the contract version this app targets (current is %d)", Version)
	case s.Platform < MinVersion:
		fail("platform %d is no longer supported; the oldest accepted is %d", s.Platform, MinVersion)
	case s.Platform > Version:
		fail("platform %d is newer than this deployment understands (%d)", s.Platform, Version)
	}

	if strings.TrimSpace(s.Build.Command) == "" {
		fail("build.command is required: a checkout is not yet something that can run")
	}
	s.validateServices(fail)
	s.validateRequires(fail)
	s.validateSecrets(fail)
	s.validateAgentSurface(fail)
	s.validateJobs(fail)
	if s.Access != nil {
		if err := s.Access.Validate(); err != nil {
			fail("access: %s", strings.TrimPrefix(err.Error(), "domain: "))
		}
	}
	s.validateExtends(fail)

	if len(problems) > 0 {
		return fmt.Errorf("appspec: %s does not satisfy the platform contract:\n  - %s",
			Filename, strings.Join(problems, "\n  - "))
	}
	return nil
}

func (s *Spec) validateServices(fail func(string, ...any)) {
	if len(s.Services) == 0 {
		fail("an app runs at least one service")
		return
	}

	seen := make(map[string]bool, len(s.Services))
	primaries := 0
	for _, service := range s.Services {
		if !slugPattern.MatchString(service.Name) {
			fail("service name %q must be a lowercase slug such as %q", service.Name, "web")
			continue
		}
		if seen[service.Name] {
			fail("two services are both named %q, and depends_on refers to services by name", service.Name)
		}
		seen[service.Name] = true

		switch domain.ServiceKind(service.Kind) {
		case domain.ServiceWeb, domain.ServiceAPI, domain.ServiceWorker, domain.ServiceDatastore:
		default:
			fail("service %q has kind %q; it must be web, api, worker or datastore", service.Name, service.Kind)
		}
		if strings.TrimSpace(service.Command) == "" {
			fail("service %q has no command, so nothing would start it", service.Name)
		}
		if service.Port < 0 || service.Port > 65535 {
			fail("service %q has port %d", service.Name, service.Port)
		}
		if service.Exposed && service.Port == 0 {
			fail("service %q is exposed but listens on nothing, so its hostname would answer no one", service.Name)
		}
		if service.HealthPath != "" && !strings.HasPrefix(service.HealthPath, "/") {
			fail("service %q has health_path %q, which must be an absolute path", service.Name, service.HealthPath)
		}
		if service.Primary {
			primaries++
			if !service.Exposed {
				fail("service %q is primary but not exposed; the front door has to be reachable", service.Name)
			}
		}
	}

	switch primaries {
	case 1:
	case 0:
		fail("no service is marked primary, so nothing names the app's front door")
	default:
		fail("%d services are marked primary; an app has one front door", primaries)
	}

	for _, service := range s.Services {
		for _, needed := range service.DependsOn {
			if !seen[needed] {
				fail("service %q depends on %q, which this app does not declare", service.Name, needed)
			}
		}
	}
}

func (s *Spec) validateRequires(fail func(string, ...any)) {
	if engine := s.Requires.Database.Engine; engine != "" && engine != "postgres" {
		// One engine means one backup story, one restore procedure and one
		// snapshot mechanism. A second buys a customer nothing they can see.
		fail("requires.database.engine is %q; the platform provides postgres", engine)
	}
	switch s.Requires.Payments {
	case "", "required", "optional":
	default:
		fail("requires.payments is %q; it must be required or optional", s.Requires.Payments)
	}
}

func (s *Spec) validateSecrets(fail func(string, ...any)) {
	seen := make(map[string]bool, len(s.Secrets))
	for _, secret := range s.Secrets {
		if !envPattern.MatchString(secret.Name) {
			fail("secret %q must be an environment variable name such as %q", secret.Name, "STRIPE_SECRET_KEY")
		}
		if seen[secret.Name] {
			fail("secret %q is declared twice", secret.Name)
		}
		seen[secret.Name] = true
		if strings.TrimSpace(secret.Description) == "" {
			// The description is what the customer is shown when asked for it.
			// Without one they are being asked for a credential by name alone.
			fail("secret %q has no description, so the install form would ask for it with no explanation", secret.Name)
		}
	}
}

func (s *Spec) validateAgentSurface(fail func(string, ...any)) {
	// Agent access is not optional and not a paid tier. An app that does not
	// expose its operations as tools cannot be driven by the customer's own
	// agent, which is most of what they are paying for.
	if s.MCP.Path == "" {
		fail("mcp.path is required: every app exposes an MCP server")
		return
	}
	if !strings.HasPrefix(s.MCP.Path, "/") {
		fail("mcp.path %q must be an absolute path", s.MCP.Path)
	}
}

func (s *Spec) validateJobs(fail func(string, ...any)) {
	seen := make(map[string]bool, len(s.Jobs))
	for _, job := range s.Jobs {
		if !slugPattern.MatchString(job.Name) {
			fail("job name %q must be a lowercase slug", job.Name)
		}
		if seen[job.Name] {
			fail("job %q is declared twice", job.Name)
		}
		seen[job.Name] = true
		if !strings.HasPrefix(job.Path, "/") {
			fail("job %q has path %q, which must be an absolute path the platform can invoke", job.Name, job.Path)
		}
		if fields := strings.Fields(job.Schedule); len(fields) != 5 {
			fail("job %q has schedule %q; it must be five cron fields", job.Name, job.Schedule)
		}
	}
}

func (s *Spec) validateExtends(fail func(string, ...any)) {
	// The extension surface is the whole design: the agent works here and core
	// stays upgradable. A path that escapes the repository would make "core"
	// mean nothing.
	for label, dir := range map[string]string{
		"theme":   s.Extends.Theme,
		"plugins": s.Extends.Plugins,
		"config":  s.Extends.Config,
	} {
		if dir == "" {
			continue
		}
		if path.IsAbs(dir) || strings.HasPrefix(path.Clean(dir), "..") {
			fail("extends.%s is %q; it must be a relative path inside the app", label, dir)
		}
	}
	if prefix := s.Extends.SchemaPrefix; prefix != "" && !prefixPattern.MatchString(prefix) {
		fail("extends.schema_prefix is %q; it must be a short lowercase prefix ending in _ such as %q",
			prefix, "ext_")
	}
}

// ErrNoPrimary reports a spec with no front door. Kept separate because the
// caller that asks for the primary service wants to say something better than
// "not found".
var ErrNoPrimary = errors.New("appspec: this app declares no primary service")

// DomainServices converts the manifest's services into the model the control
// plane already runs, so an app declared by a manifest and a project
// discovered in a repository start by exactly the same path.
func (s *Spec) DomainServices(projectID string) []*domain.Service {
	services := make([]*domain.Service, 0, len(s.Services))
	for _, declared := range s.Services {
		services = append(services, &domain.Service{
			ProjectID:  projectID,
			Name:       declared.Name,
			Kind:       domain.ServiceKind(declared.Kind),
			Command:    declared.Command,
			Port:       declared.Port,
			HealthPath: declared.HealthPath,
			Exposed:    declared.Exposed,
			Primary:    declared.Primary,
			DependsOn:  append([]string(nil), declared.DependsOn...),
			// Declared by the app's author rather than inferred by a discovery
			// pass, so there is nothing for a human to confirm.
			Confirmed: true,
		})
	}
	return services
}

// PrimaryService returns the app's front door.
func (s *Spec) PrimaryService() (Service, error) {
	for _, service := range s.Services {
		if service.Primary {
			return service, nil
		}
	}
	return Service{}, ErrNoPrimary
}
