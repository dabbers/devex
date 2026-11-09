package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BinaryEnv names the environment variable that points at the browser to run.
const BinaryEnv = "DABBERZ_BROWSER"

// candidates are the browser binaries to try when none is configured, in the
// order they are preferred. The first is where the session image keeps the
// Chromium that Playwright manages.
// The absolute paths come first and are tried even when PATH is empty, which
// is what a command run through a VM driver can end up with.
var candidates = []string{
	"/opt/pw-browsers/chromium",
	"/usr/bin/chromium",
	"/usr/bin/chromium-browser",
	"/usr/bin/google-chrome",
	"/usr/bin/google-chrome-stable",
	"google-chrome",
	"chromium",
	"chromium-browser",
	"chrome",
}

// startupTimeout bounds how long the browser gets to open its DevTools port.
const startupTimeout = 30 * time.Second

// Options configures a launch.
type Options struct {
	// Binary overrides the browser to run. Empty consults BinaryEnv and then
	// the candidate list.
	Binary string
	// Headful runs the browser with a visible window. It requires a display;
	// the default follows DISPLAY, so a UI VM with an X server gets a real
	// window and a machine without one still works.
	Headful bool
	// Width and Height size the viewport. Zero uses a desktop default.
	Width, Height int
	// UserDataDir holds the profile. Empty creates a temporary one that is
	// removed on Close.
	UserDataDir string
}

// Browser is a running browser process with a DevTools connection to its page.
type Browser struct {
	cmd     *exec.Cmd
	client  *Client
	dataDir string
	// ownsDataDir records whether Close should remove the profile.
	ownsDataDir bool
}

// Find returns the browser binary to run, or an error naming what was tried.
func Find(binary string) (string, error) {
	if binary == "" {
		binary = os.Getenv(BinaryEnv)
	}
	if binary != "" {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			return "", fmt.Errorf("browser: %q is not runnable: %w", binary, err)
		}
		return resolved, nil
	}
	for _, candidate := range candidates {
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("browser: no browser found; set %s to one (tried %s)",
		BinaryEnv, strings.Join(candidates, ", "))
}

// Launch starts a browser and connects to its first page.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	binary, err := Find(opts.Binary)
	if err != nil {
		return nil, err
	}

	dataDir, ownsDataDir := opts.UserDataDir, false
	if dataDir == "" {
		dataDir, err = os.MkdirTemp("", "dabberz-browser-")
		if err != nil {
			return nil, fmt.Errorf("browser: profile directory: %w", err)
		}
		ownsDataDir = true
	}

	width, height := opts.Width, opts.Height
	if width <= 0 || height <= 0 {
		width, height = 1280, 900
	}

	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + dataDir,
		fmt.Sprintf("--window-size=%d,%d", width, height),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--disable-extensions",
		"--disable-dev-shm-usage",
		// A verification runs inside a VM that is already the isolation
		// boundary, and the browser's own sandbox needs privileges a container
		// does not have.
		"--no-sandbox",
		"about:blank",
	}
	headful := opts.Headful || os.Getenv("DISPLAY") != ""
	if !headful {
		args = append([]string{"--headless=new"}, args...)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	// The browser is torn down by Close; giving it its own process group keeps
	// a stray signal to the harness from killing it mid-screenshot.
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		if ownsDataDir {
			_ = os.RemoveAll(dataDir)
		}
		return nil, fmt.Errorf("browser: start %s: %w", binary, err)
	}

	browser := &Browser{cmd: cmd, dataDir: dataDir, ownsDataDir: ownsDataDir}

	port, err := awaitDevToolsPort(ctx, dataDir)
	if err != nil {
		_ = browser.Close()
		return nil, err
	}
	wsURL, err := awaitPageTarget(ctx, port)
	if err != nil {
		_ = browser.Close()
		return nil, err
	}
	client, err := Dial(wsURL, 10*time.Second)
	if err != nil {
		_ = browser.Close()
		return nil, err
	}
	browser.client = client
	return browser, nil
}

// Client returns the DevTools connection to the browser's page.
func (b *Browser) Client() *Client { return b.client }

// Close shuts the browser down and removes a profile it created.
func (b *Browser) Close() error {
	if b.client != nil {
		_ = b.client.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
	if b.ownsDataDir {
		return os.RemoveAll(b.dataDir)
	}
	return nil
}

// awaitDevToolsPort reads the port the browser chose.
//
// Asking for port 0 and reading back what was picked is what makes concurrent
// verifications on one machine safe: nothing has to agree a port in advance.
func awaitDevToolsPort(ctx context.Context, dataDir string) (int, error) {
	path := filepath.Join(dataDir, "DevToolsActivePort")
	deadline := time.Now().Add(startupTimeout)
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			line, _, _ := strings.Cut(strings.TrimSpace(string(contents)), "\n")
			if port, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && port > 0 {
				return port, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("browser: read devtools port: %w", err)
		}

		if time.Now().After(deadline) {
			return 0, fmt.Errorf("browser: it did not open a devtools port within %s", startupTimeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// awaitPageTarget returns the websocket URL of the browser's page.
//
// Attaching to the page directly, rather than to the browser and then
// attaching a session to a target, is the shorter path: one connection, no
// session identifiers threaded through every command.
func awaitPageTarget(ctx context.Context, port int) (string, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(startupTimeout)

	for {
		wsURL, err := pageTarget(ctx, client, endpoint)
		if err == nil {
			return wsURL, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("browser: no page target within %s: %w", startupTimeout, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// pageTarget asks the browser for its targets and picks the page.
func pageTarget(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("devtools target list returned %s", resp.Status)
	}

	var targets []struct {
		Type string `json:"type"`
		URL  string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", err
	}
	for _, target := range targets {
		if target.Type == "page" && target.URL != "" {
			return target.URL, nil
		}
	}
	return "", errors.New("the browser has no page target yet")
}
