package appspec

import (
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
)

// valid is a manifest that satisfies the contract, used as the base for the
// refusal tests so each one varies exactly one thing.
const valid = `
app: forms
name: Forms
version: 1.0.0
platform: 1
license: AGPL-3.0
upstream: https://github.com/dabbers/app-forms
build:
  command: go build -o forms .
services:
  - name: web
    kind: web
    command: forms serve
    port: 3000
    health_path: /healthz
    primary: true
    exposed: true
  - name: worker
    kind: worker
    command: forms work
    depends_on: [web]
requires:
  database:
    engine: postgres
    version: "16"
  identity: true
  mail: true
secrets:
  - name: TURNSTILE_SECRET
    description: Spam protection for public submissions.
    required: false
mcp:
  path: /mcp
  tools: tools/
jobs:
  - name: digest
    schedule: "0 13 * * 1"
    path: /jobs/digest
extends:
  theme: theme/
  plugins: plugins/
  config: config/
  schema_prefix: ext_
checks:
  suite: checks/
`

func TestAConformingManifestIsAccepted(t *testing.T) {
	t.Parallel()

	spec, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.App != "forms" || spec.Platform != Version {
		t.Fatalf("spec = %+v", spec)
	}

	primary, err := spec.PrimaryService()
	if err != nil {
		t.Fatalf("PrimaryService: %v", err)
	}
	if primary.Name != "web" {
		t.Fatalf("primary = %q, want web", primary.Name)
	}
}

// TestTheManifestFeedsTheEngineThatAlreadyExists: an app declared by a
// manifest and a project discovered in a repository have to start by exactly
// the same path, or the half that gets less use is the half that breaks.
func TestTheManifestFeedsTheEngineThatAlreadyExists(t *testing.T) {
	t.Parallel()

	spec, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	services := spec.DomainServices("prj_1")
	if len(services) != 2 {
		t.Fatalf("got %d services", len(services))
	}
	for _, service := range services {
		if service.ProjectID != "prj_1" {
			t.Errorf("service %q has project %q", service.Name, service.ProjectID)
		}
		if !service.Confirmed {
			t.Errorf("service %q needs confirming, but its author declared it", service.Name)
		}
	}

	// The domain model's own rules have to accept what the manifest produced,
	// including dependency ordering.
	order, err := domain.StartOrder(services)
	if err != nil {
		t.Fatalf("StartOrder: %v", err)
	}
	if len(order) != 2 || order[0].Name != "web" {
		t.Fatalf("start order = %v, want web before worker", names(order))
	}
	for _, service := range services {
		if err := service.Validate(); err != nil {
			t.Errorf("service %q: %v", service.Name, err)
		}
	}
}

func names(services []*domain.Service) []string {
	out := make([]string, 0, len(services))
	for _, service := range services {
		out = append(out, service.Name)
	}
	return out
}

// TestTheContractIsEnforced walks the rules in docs/PLATFORM.md one at a time.
// Each case changes exactly one thing about a manifest that is otherwise
// valid, so a failure names the rule that stopped working.
func TestTheContractIsEnforced(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		manifest string
		says     string
	}{
		"an unknown field is a typo, not an extension": {
			//nolint:misspell // the misspelling is the case under test
			manifest: valid + "\nplatfrom: 1\n",
			says:     "not a readable manifest",
		},
		"no primary service names no front door": {
			manifest: strings.Replace(valid, "    primary: true\n", "", 1),
			says:     "no service is marked primary",
		},
		"two front doors is not a front door": {
			manifest: strings.Replace(valid,
				"    command: forms work\n",
				"    command: forms work\n    primary: true\n    exposed: true\n    port: 4000\n", 1),
			says: "2 services are marked primary",
		},
		"a primary nobody can reach": {
			manifest: strings.Replace(valid, "    exposed: true\n", "    exposed: false\n", 1),
			says:     "primary but not exposed",
		},
		"exposing a service that listens on nothing": {
			manifest: strings.Replace(valid,
				"    command: forms work\n",
				"    command: forms work\n    exposed: true\n", 1),
			says: "listens on nothing",
		},
		"no build step": {
			manifest: strings.Replace(valid, "  command: go build -o forms .\n", "", 1),
			says:     "build.command is required",
		},
		"a service with no command starts nothing": {
			manifest: strings.Replace(valid, "    command: forms serve\n", "", 1),
			says:     "has no command",
		},
		"depending on a service that does not exist": {
			manifest: strings.Replace(valid, "depends_on: [web]", "depends_on: [database]", 1),
			says:     "which this app does not declare",
		},
		"a second database engine": {
			manifest: strings.Replace(valid, "engine: postgres", "engine: mysql", 1),
			says:     "the platform provides postgres",
		},
		"a secret with no explanation": {
			manifest: strings.Replace(valid,
				"    description: Spam protection for public submissions.\n", "", 1),
			says: "would ask for it with no explanation",
		},
		"a secret that is not an environment variable": {
			manifest: strings.Replace(valid, "name: TURNSTILE_SECRET", "name: turnstile-secret", 1),
			says:     "must be an environment variable name",
		},
		"no agent surface": {
			manifest: strings.Replace(valid, "  path: /mcp\n", "", 1),
			says:     "every app exposes an MCP server",
		},
		"a schedule that is not a schedule": {
			manifest: strings.Replace(valid, `schedule: "0 13 * * 1"`, `schedule: "weekly"`, 1),
			says:     "five cron fields",
		},
		"an extension surface that escapes the app": {
			manifest: strings.Replace(valid, "theme: theme/", "theme: ../../etc/", 1),
			says:     "relative path inside the app",
		},
		"a schema prefix that would collide with core": {
			manifest: strings.Replace(valid, "schema_prefix: ext_", "schema_prefix: public", 1),
			says:     "short lowercase prefix ending in _",
		},
		"a contract version from the future": {
			manifest: strings.Replace(valid, "platform: 1", "platform: 99", 1),
			says:     "newer than this deployment understands",
		},
		"no contract version at all": {
			manifest: strings.Replace(valid, "platform: 1\n", "", 1),
			says:     "must declare the contract version",
		},
		"an app with no name in the catalog": {
			manifest: strings.Replace(valid, "name: Forms\n", "", 1),
			says:     "cannot be empty",
		},
		"a version nothing can pin": {
			manifest: strings.Replace(valid, "version: 1.0.0\n", "", 1),
			says:     "what an installation pins",
		},
		"a service kind the platform cannot run": {
			manifest: strings.Replace(valid, "    kind: web\n", "    kind: lambda\n", 1),
			says:     "web, api, worker or datastore",
		},
		"a relative health path": {
			manifest: strings.Replace(valid, "health_path: /healthz", "health_path: healthz", 1),
			says:     "must be an absolute path",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(test.manifest))
			if err == nil {
				t.Fatalf("the manifest was accepted; it should have been refused for %q", test.says)
			}
			if !strings.Contains(err.Error(), test.says) {
				t.Fatalf("refused for the wrong reason:\n%v\nwanted it to mention %q", err, test.says)
			}
		})
	}
}

// TestEveryProblemIsReportedAtOnce: an author fixing a manifest one refusal at
// a time is a slow loop, and the platform already knows all of them.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	t.Parallel()

	broken := strings.NewReplacer(
		"engine: postgres", "engine: mysql",
		"  path: /mcp\n", "",
		`schedule: "0 13 * * 1"`, `schedule: "weekly"`,
	).Replace(valid)

	_, err := Parse([]byte(broken))
	if err == nil {
		t.Fatal("a manifest with three faults was accepted")
	}
	for _, expected := range []string{"postgres", "MCP server", "cron fields"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the report does not mention %q:\n%v", expected, err)
		}
	}
}
