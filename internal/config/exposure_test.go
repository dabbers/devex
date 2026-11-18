package config

import (
	"github.com/dabbers/devex/internal/auth"
	"strings"
	"testing"
)

// TestSingleUserAuthIsRefusedOnAPublicAddress is the one misconfiguration that
// does not announce itself.
//
// Single-user auth accepts anything. On an address the internet can reach,
// that hands a shell, the secrets vault and the audit trail to whoever
// connects -- while the deployment looks like it is working, which is why a
// warning is the wrong instrument for it.
func TestSingleUserAuthIsRefusedOnAPublicAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:80", ":80", "147.135.70.81:80", "[::]:443"} {
		cfg := &Config{
			Owner: "someone@example.com", DataDir: "/var/lib/dabberz",
			Server: ServerConfig{Addr: addr},
			Auth:   AuthConfig{Kind: AuthSingle},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("server.addr %s with single-user auth was accepted", addr)
		}
		if !strings.Contains(err.Error(), "not loopback") {
			t.Fatalf("server.addr %s refused for the wrong reason: %v", addr, err)
		}
	}
}

// And the two ways out both work, or the refusal is just an obstacle.
func TestTheWaysOutOfThatRefusalWork(t *testing.T) {
	loopback := &Config{
		Owner: "someone@example.com", DataDir: "/var/lib/dabberz",
		Server: ServerConfig{Addr: "127.0.0.1:8090"},
		Auth:   AuthConfig{Kind: AuthSingle},
	}
	// Checked for the absence of this particular objection rather than for a
	// valid config: these fixtures leave out plenty of other required fields,
	// and the point here is only that binding to loopback answers it.
	if err := loopback.Validate(); err != nil && strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("single-user auth on loopback was still refused for exposure: %v", err)
	}

	provider := &Config{
		Owner: "someone@example.com", DataDir: "/var/lib/dabberz",
		Server: ServerConfig{Addr: "0.0.0.0:80"},
		Auth: AuthConfig{Kind: AuthClerk, Clerk: auth.ClerkConfig{
			PublishableKey: "pk_test_x", SecretKey: "sk_test_x",
		}},
	}
	if err := provider.Validate(); err != nil && strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("a public address with a provider was refused for exposure: %v", err)
	}
}

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8090": true, "localhost:80": true, "[::1]:443": true,
		"127.0.0.1": true, "::1": true,
		"0.0.0.0:80": false, ":80": false, "[::]:80": false,
		"147.135.70.81:80": false, "panel.dabberz.com:443": false, "": false,
	} {
		if got := loopbackAddr(addr); got != want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
