package workspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// Exec runs a command on a machine with no environment of the workspace's own:
// for the platform's bookkeeping in a workspace, never for anything of the
// app's, which runs with its secrets through Run.
func (m *Manager) Exec(ctx context.Context, instanceID string, cmd vm.Command) (*vm.ExecResult, error) {
	cmd.Env = nil
	return m.driver.Exec(ctx, instanceID, cmd)
}

// cleanRelative refuses a path that is absolute or climbs out of the workspace.
func cleanRelative(rel string) (string, error) {
	clean := path.Clean(rel)
	if rel == "" || path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("workspace: %q is not a path inside the workspace", rel)
	}
	return clean, nil
}

// ReadFile returns a file from the workspace, up to limit bytes. A file that is
// not there is os.ErrNotExist; one that is larger than limit is an error.
func (m *Manager) ReadFile(ctx context.Context, w *domain.Workspace, rel string, limit int) ([]byte, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}
	if w.InstanceID == "" {
		return nil, fmt.Errorf("workspace: %s has no machine", w.ID)
	}
	// Base64 because a command's output is text, and a file need not be.
	script := fmt.Sprintf(`[ -f %[1]s ] && [ ! -L %[1]s ] || exit 3; head -c %[2]d %[1]s | base64 -w0`,
		shellQuote(clean), limit+1)
	res, err := m.Exec(ctx, w.InstanceID, vm.Command{Argv: []string{"sh", "-c", script}, Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 3 {
		return nil, fmt.Errorf("workspace: %s: %w", clean, os.ErrNotExist)
	}
	if !res.OK() {
		return nil, fmt.Errorf("workspace: read %s: %s", clean, firstLine(res.Stderr))
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("workspace: read %s: %w", clean, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("workspace: %s is larger than %d bytes", clean, limit)
	}
	return data, nil
}

// WriteFile puts a read-only file into the workspace, replacing it whole, so
// whatever reads it sees the old file or the new one and never half of each.
func (m *Manager) WriteFile(ctx context.Context, w *domain.Workspace, rel string, data []byte) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	if w.InstanceID == "" {
		return fmt.Errorf("workspace: %s has no machine", w.ID)
	}
	tmp := clean + ".writing"
	script := fmt.Sprintf(`set -e; mkdir -p %s; rm -f %s; cat > %s; chmod 0444 %s; mv -f %s %s`,
		shellQuote(path.Dir(clean)), shellQuote(tmp), shellQuote(tmp), shellQuote(tmp),
		shellQuote(tmp), shellQuote(clean))
	res, err := m.Exec(ctx, w.InstanceID, vm.Command{
		Argv: []string{"sh", "-c", script}, Stdin: data, Timeout: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("workspace: write %s: %s", clean, firstLine(res.Stderr))
	}
	return nil
}
