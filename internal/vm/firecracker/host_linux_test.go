//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTotalMemoryMiBParsesMeminfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	body := "MemTotal:       263856128 kB\nMemFree:         1234 kB\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := totalMemoryMiB(path)
	if err != nil {
		t.Fatalf("totalMemoryMiB: %v", err)
	}
	if want := 263856128 / 1024; got != want {
		t.Fatalf("totalMemoryMiB = %d, want %d", got, want)
	}
}

func TestTotalMemoryMiBRejectsMalformedInput(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"no MemTotal line":  "MemFree: 100 kB\n",
		"unparseable value": "MemTotal:  not-a-number kB\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := totalMemoryMiB(path); err == nil {
			t.Errorf("totalMemoryMiB(%s) = nil, want an error", name)
		}
	}
	if _, err := totalMemoryMiB(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("totalMemoryMiB(missing file) should fail")
	}
}

func TestProbeHostReportsSomethingPlausible(t *testing.T) {
	got, err := probeHost()
	if err != nil {
		t.Skipf("host probe unavailable in this environment: %v", err)
	}
	if got.VCPUs <= 0 || got.MemoryMiB <= 0 {
		t.Fatalf("probeHost returned implausible capacity: %s", got)
	}
}
