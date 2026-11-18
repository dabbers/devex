package config

import (
	"os"
	"strings"
	"testing"
)

func TestDbzAlphaConfigLoads(t *testing.T) {
	t.Setenv("DABBERZ_MASTER_KEY", strings.Repeat("a", 44))
	t.Setenv("DABBERZ_CLERK_SECRET", "sk_test_placeholder")
	// From the constant, not spelled out again. Spelling it out is how this
	// test passed while the bootstrap wrote a name nothing reads: the config
	// simply had no DSN, and a config with no data block still validates.
	t.Setenv(EnvDataAdminDSN, "postgres://dabberz:x@127.0.0.1:5432/postgres?sslmode=disable")

	raw, err := os.ReadFile("../../deploy/dbz-alpha.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The two marked placeholders, as an operator would fill them.
	filled := strings.ReplaceAll(string(raw), "CHANGE-ME@example.com", "someone@example.com")
	filled = strings.ReplaceAll(filled, "publishable_key: CHANGE-ME", "publishable_key: pk_test_eW91ci1pbnN0YW5jZS5jbGVyay5hY2NvdW50cy5kZXYk")

	path := t.TempDir() + "/dabberz.yaml"
	if err := os.WriteFile(path, []byte(filled), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the deployment config does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the deployment config does not validate: %v", err)
	}
	for _, w := range cfg.Warnings() {
		t.Logf("warning: %s", w)
	}
	if cfg.Server.Addr != "0.0.0.0:80" {
		t.Errorf("server.addr = %q", cfg.Server.Addr)
	}
	if cfg.Preview.Domain != "ondabberz.link" {
		t.Errorf("preview.domain = %q", cfg.Preview.Domain)
	}
	// The whole point of the environment variable. Without this the test
	// passes for a deployment that cannot install anything with a database.
	if cfg.Data.AdminDSN == "" {
		t.Error("the data block is unconfigured; apps needing a database cannot be installed")
	}
}
