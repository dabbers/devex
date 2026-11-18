package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryEnvironmentVariableNamedInDocsIsReal catches the mistake that has
// now been made three times in one deployment.
//
// A credential put in the environment under a name nothing reads does not
// fail. The daemon starts, reports itself healthy, and behaves as though the
// value were simply absent -- which for a DSN meant "apps needing a database
// cannot be installed", and for a provider secret means attribution quietly
// not working. The name is the whole interface, and prose is where it drifts.
func TestEveryEnvironmentVariableNamedInDocsIsReal(t *testing.T) {
	// Built from the constants, so renaming one breaks the build here rather
	// than leaving the documents describing a name that no longer exists.
	real := map[string]bool{
		EnvMasterKey:        true,
		EnvOrchestrator:     true,
		EnvDataAdminDSN:     true,
		EnvStorageSecret:    true,
		EnvClaudeToken:      true,
		EnvClerkPublishable: true,
		EnvClerkSecret:      true,
		// Read by the agent rather than by this package, and named in the
		// same documents.
		"CLAUDE_CONFIG_DIR": true,
		// Set for an app by the platform, documented as part of the contract.
		"DABBERZ_TRUSTED_PROXY":      true,
		"DABBERZ_CHECK_URL":          true,
		"DABBERZ_CHECK_DATABASE_URL": true,
		"DATABASE_URL":               true,
		// The command line tool's own.
		"DABBERZ_TOKEN":    true,
		"DABBERZ_ENDPOINT": true,
		// Test fixtures, named in the host document's testing notes.
		"DABBERZ_TEST_CLUSTER_DSN": true,
		"DABBERZ_TEST_PG_BIN":      true,
		// Not an environment variable at all: the bootstrap script's own
		// shell constant for the signing key fingerprint. Listed rather than
		// pattern-matched away, so the next one that looks like this has to
		// be looked at.
		"CLAUDE_KEY_FPR": true,
	}

	shaped := regexp.MustCompile(`\b(?:DABBERZ|CLERK|CLAUDE)_[A-Z0-9_]{2,}\b`)
	var problems []string

	for _, path := range []string{
		"../../deploy/dbz-alpha.yaml",
		"../../deploy/bootstrap-host.sh",
		"../../deploy/dabberzd.service",
		"../../configs/dabberz.example.yaml",
		"../../docs/HOST.md",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		seen := map[string]bool{}
		for _, name := range shaped.FindAllString(string(body), -1) {
			if real[name] || seen[name] {
				continue
			}
			seen[name] = true
			problems = append(problems,
				filepath.Base(path)+" names "+name+", which nothing reads")
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("environment variables named in documents but read by nothing:\n  %s",
			strings.Join(problems, "\n  "))
	}
}
