package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The endpoint has to come from the daemon's own configuration, or a default
// port is right only until somebody changes server.addr -- after which every
// command fails with connection refused and nothing says the two disagree.
func TestTheEndpointComesFromWhereTheDaemonListens(t *testing.T) {
	for name, tc := range map[string]struct{ addr, want string }{
		"every interface on 80":   {"0.0.0.0:80", "http://127.0.0.1"},
		"every interface on 8080": {"0.0.0.0:8080", "http://127.0.0.1:8080"},
		"loopback":                {"127.0.0.1:8090", "http://127.0.0.1:8090"},
		"no host":                 {":80", "http://127.0.0.1"},
		"ipv6 any":                {"[::]:80", "http://127.0.0.1"},
		"a named address":         {"10.0.0.5:9000", "http://10.0.0.5:9000"},
		"not an address":          {"nonsense", ""},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dabberz.yaml")
			// Quoted, because an IPv6 address unquoted in YAML starts a
			// flow sequence rather than a string.
			body := "server:\n  addr: \"" + tc.addr + "\"\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := listenAddrFrom(path); got != tc.want {
				t.Errorf("listenAddrFrom(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// No config here means no opinion, rather than a wrong one.
func TestNoConfigMeansNoOpinion(t *testing.T) {
	if got := listenAddrFrom(filepath.Join(t.TempDir(), "absent.yaml")); got != "" {
		t.Errorf("an absent config produced %q", got)
	}
	path := filepath.Join(t.TempDir(), "dabberz.yaml")
	if err := os.WriteFile(path, []byte("owner: someone@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listenAddrFrom(path); got != "" {
		t.Errorf("a config with no server.addr produced %q", got)
	}
}

// And an explicit endpoint still wins, so this works off the host too.
func TestAnExplicitEndpointWins(t *testing.T) {
	t.Setenv("DABBERZ_ENDPOINT", "https://panel.dabberz.com/")
	if got := endpoint(); got != "https://panel.dabberz.com" {
		t.Errorf("endpoint() = %q", got)
	}
}
