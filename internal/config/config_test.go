package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/vm"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dabberz.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaultsForOmittedKeys(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	path := writeConfig(t, "owner: me@example.com\npreview:\n  domain: dab.im\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Owner != "me@example.com" {
		t.Fatalf("owner = %q", cfg.Owner)
	}
	// Anything the file omits keeps its default rather than becoming a zero
	// value that would fail at runtime.
	if cfg.Server.Addr == "" || cfg.DataDir == "" {
		t.Fatalf("defaults were not applied: %+v", cfg)
	}
	if cfg.Tripwire.MaxCycles == 0 {
		t.Fatal("tripwire defaults were not applied")
	}
	if cfg.Driver.Kind != "local" {
		t.Fatalf("driver kind = %q, want the local default", cfg.Driver.Kind)
	}
}

func TestLoadParsesDurationsAndNestedSections(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	path := writeConfig(t, `
owner: me@example.com
preview:
  domain: dab.im
  port_start: 50000
  port_end: 50100
scheduler:
  interval: 30s
  head_of_line_blocking: true
tripwire:
  max_cycles: 3
  max_wall_clock: 90m
verify:
  ui_instance_id: vm_ui
  profiles: 8
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scheduler.Interval != 30*time.Second || !cfg.Scheduler.HeadOfLineBlocking {
		t.Fatalf("scheduler = %+v", cfg.Scheduler)
	}
	if cfg.Tripwire.MaxWallClock != 90*time.Minute || cfg.Tripwire.MaxCycles != 3 {
		t.Fatalf("tripwire = %+v", cfg.Tripwire)
	}
	if cfg.Preview.PortStart != 50000 || cfg.Preview.PortEnd != 50100 {
		t.Fatalf("preview = %+v", cfg.Preview)
	}
	if cfg.Verify.Profiles != 8 || cfg.Verify.UIInstanceID != "vm_ui" {
		t.Fatalf("verify = %+v", cfg.Verify)
	}
}

func TestNestedResourceKeysAreHonoured(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	// Every documented key must actually take effect. A struct carrying only
	// json tags would quietly fall back to its default here, which reads as
	// the machine having more capacity than it was given.
	cfg, err := Load(writeConfig(t, `
owner: me@example.com
preview:
  domain: dab.im
driver:
  fork_resources:
    vcpus: 3
    memory_mib: 6144
    disk_gib: 30
  local:
    total:
      vcpus: 12
      memory_mib: 24576
      disk_gib: 500
    max_instances: 7
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := vm.Resources{VCPUs: 3, MemoryMiB: 6144, DiskGiB: 30}
	if cfg.Driver.ForkResources != want {
		t.Errorf("fork_resources = %+v, want %+v", cfg.Driver.ForkResources, want)
	}
	wantTotal := vm.Resources{VCPUs: 12, MemoryMiB: 24576, DiskGiB: 500}
	if cfg.Driver.Local.Total != wantTotal {
		t.Errorf("local.total = %+v, want %+v", cfg.Driver.Local.Total, wantTotal)
	}
	if cfg.Driver.Local.MaxInstances != 7 {
		t.Errorf("max_instances = %d, want 7", cfg.Driver.Local.MaxInstances)
	}
}

func TestFirecrackerResourceKeysAreHonoured(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	cfg, err := Load(writeConfig(t, `
owner: me@example.com
preview:
  domain: dab.im
driver:
  kind: firecracker
  firecracker:
    kernel_image: /srv/vmlinux
    golden_rootfs: /srv/golden.ext4
    host_reserve:
      vcpus: 4
      memory_mib: 8192
      disk_gib: 100
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Held-back capacity that silently read as zero would let fork VMs crowd
	// out the control plane itself.
	want := vm.Resources{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 100}
	if cfg.Driver.Firecracker.HostReserve != want {
		t.Fatalf("host_reserve = %+v, want %+v", cfg.Driver.Firecracker.HostReserve, want)
	}
}

func TestEnvironmentOverridesTheFileForCredentials(t *testing.T) {
	path := writeConfig(t, `
owner: me@example.com
master_key: aaaa
preview:
  domain: dab.im
llm:
  api_key: from-file
agent:
  oauth_token: from-file
`)
	key := strings.Repeat("cd", 32)
	t.Setenv(EnvMasterKey, key)
	t.Setenv(EnvOrchestrator, "from-env")
	t.Setenv(EnvClaudeToken, "token-from-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Credentials come from the environment so nothing secret has to live in
	// a file that is meant to be readable.
	if cfg.MasterKey != key {
		t.Fatalf("master key = %q, want the environment's", cfg.MasterKey)
	}
	if cfg.LLM.APIKey != "from-env" || cfg.Agent.OAuthToken != "token-from-env" {
		t.Fatalf("credentials were not overridden: %+v %+v", cfg.LLM, cfg.Agent)
	}
}

func TestValidateRejectsAnUnusableConfiguration(t *testing.T) {
	tests := map[string]string{
		"no master key":  "owner: me@example.com\npreview:\n  domain: dab.im\n",
		"no owner":       "owner: \"\"\npreview:\n  domain: dab.im\n",
		"no domain":      "owner: me@example.com\npreview:\n  domain: \"\"\n",
		"unknown driver": "owner: me@example.com\npreview:\n  domain: dab.im\ndriver:\n  kind: podman\n",
	}
	for name, body := range tests {
		if name != "no master key" {
			t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
		} else {
			t.Setenv(EnvMasterKey, "")
		}
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("Load(%s) should have failed validation", name)
		}
	}
}

func TestMissingMasterKeyExplainsHowToFixIt(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	_, err := Load(writeConfig(t, "owner: me@example.com\npreview:\n  domain: dab.im\n"))
	if err == nil {
		t.Fatal("expected a missing master key to fail")
	}
	if !strings.Contains(err.Error(), "keygen") {
		t.Fatalf("the error should say how to produce a key: %v", err)
	}
}

func TestFirecrackerDriverIsValidated(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	// A firecracker deployment with no kernel or rootfs cannot boot anything,
	// and should say so at startup rather than at the first fork.
	_, err := Load(writeConfig(t, `
owner: me@example.com
preview:
  domain: dab.im
driver:
  kind: firecracker
`))
	if err == nil {
		t.Fatal("an incomplete firecracker configuration should be rejected")
	}
	if !strings.Contains(err.Error(), "kernel_image") {
		t.Fatalf("the error should name the missing field: %v", err)
	}
}

func TestLoadReportsAMissingOrMalformedFile(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("a missing config file should be reported")
	}
	if _, err := Load(writeConfig(t, "owner: [this is not a string\n")); err == nil {
		t.Error("malformed YAML should be reported")
	}
}

func TestWarningsNameEveryLimitingGap(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	t.Setenv(EnvOrchestrator, "")
	t.Setenv(EnvClaudeToken, "")

	cfg, err := Load(writeConfig(t, "owner: me@example.com\npreview:\n  domain: dab.im\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	warnings := strings.Join(cfg.Warnings(), "\n")
	// A configuration can be valid and still unable to do the job; the daemon
	// should say which parts are inert rather than failing later.
	for _, want := range []string{"orchestrator API key", "Claude Code token", "local VM driver"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("warnings do not mention %q:\n%s", want, warnings)
		}
	}
	// The UI VM is provisioned automatically by default, so its absence from
	// the config is not a gap worth warning about.
	if strings.Contains(warnings, "shared UI VM") {
		t.Errorf("warned about the UI VM despite auto-provisioning being on:\n%s", warnings)
	}
}

func TestDisablingUIVMProvisioningIsWarnedAbout(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	cfg, err := Load(writeConfig(t, `
owner: me@example.com
preview:
  domain: dab.im
verify:
  vm:
    auto_provision: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// With no UI VM and no way to get one, nothing can be verified and so
	// nothing can reach the merge gate. That has to be said out loud.
	warnings := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warnings, "shared UI VM") {
		t.Fatalf("turning off provisioning should warn:\n%s", warnings)
	}
}

func TestAFullyConfiguredDeploymentWarnsAboutNothing(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	t.Setenv(EnvOrchestrator, "key")
	t.Setenv(EnvClaudeToken, "token")

	cfg, err := Load(writeConfig(t, `
owner: me@example.com
preview:
  domain: dab.im
caddy:
  wildcard_domain: dab.im
verify:
  ui_instance_id: vm_ui
  vm:
    auto_provision: false
driver:
  kind: firecracker
  firecracker:
    kernel_image: /srv/vmlinux
    golden_rootfs: /srv/golden.ext4
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Fatalf("a complete deployment still warned: %v", warnings)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv(EnvMasterKey, strings.Repeat("ab", 32))
	// The shipped example must actually load, or it is worse than no example.
	cfg, err := Load("../../configs/dabberz.example.yaml")
	if err != nil {
		t.Fatalf("the example configuration does not load: %v", err)
	}
	if cfg.Driver.Kind != "firecracker" {
		t.Fatalf("the example should default to the isolating driver, got %q", cfg.Driver.Kind)
	}
	if cfg.Merge.SkipQualityGate {
		t.Fatal("the example should not ship with the quality gate disabled")
	}
}
