package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/browser"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/local"
)

// buildHarness compiles this command the way a deployment ships it, so the
// integration test exercises the real binary rather than the functions behind
// it. The wire contract between the control plane and the harness is a pipe
// carrying JSON, and a pipe is the only honest way to test one.
func buildHarness(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "dabberz-verify")
	build := exec.Command("go", "build", "-o", path, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building the harness: %v", err)
	}
	return path
}

// TestTheControlPlaneCanVerifyAPreview drives the whole loop: the verifier
// hands a job to a VM driver, the driver runs the harness, the harness drives
// a browser at a real preview, and the verifier reads the verdict back.
//
// Each piece is tested on its own elsewhere. This is here because the places
// these parts meet -- the JSON on stdin, the JSON on stdout, the environment a
// command inherits from a driver -- are where they drift apart, and a loop
// that is wired but not connected looks exactly like one that works.
func TestTheControlPlaneCanVerifyAPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the harness and starts a browser")
	}
	requireBrowser(t)

	harness := buildHarness(t)
	server := previewServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	driver, err := local.New(local.Options{
		Root:  t.TempDir(),
		Total: vm.Resources{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 20},
	})
	if err != nil {
		t.Fatalf("local driver: %v", err)
	}

	instance, err := verify.EnsureUIVM(ctx, driver, verify.VMConfig{
		Resources: vm.Resources{VCPUs: 1, MemoryMiB: 1024, DiskGiB: 2},
		Env:       map[string]string{"DABBERZ_VERIFY_ARTIFACTS": t.TempDir()},
	})
	if err != nil {
		t.Fatalf("provisioning the UI VM: %v", err)
	}

	verifier, err := verify.New(driver, verify.Config{
		UIInstanceID:  instance.ID,
		Profiles:      2,
		DriverCommand: []string{harness},
		Timeout:       2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	t.Run("a preview that works", func(t *testing.T) {
		report, err := verifier.Verify(ctx, verify.Request{
			ForkID:     "fork-ok",
			PreviewURL: server.URL + "/healthy",
			Intent:     "show something a user can read",
		})
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if !report.Passed {
			t.Fatalf("a working preview failed: %s", report.FeedbackText())
		}
		if report.Profile == "" {
			t.Error("the report does not say which profile ran it")
		}
		if len(report.Checks) == 0 {
			t.Error("a passing report listed no checks")
		}
	})

	t.Run("a preview that is broken feeds the reason back", func(t *testing.T) {
		report, err := verifier.Verify(ctx, verify.Request{
			ForkID:     "fork-broken",
			PreviewURL: server.URL + "/throws",
			Intent:     "add a logout button",
		})
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if report.Passed {
			t.Fatal("a page throwing an uncaught exception passed verification")
		}

		// The feedback is what the coding agent is handed, so it has to name
		// the failure rather than just report one.
		feedback := report.FeedbackText()
		if !strings.Contains(feedback, "FAILED no_page_errors") {
			t.Fatalf("the agent would not learn what broke: %q", feedback)
		}
		if !strings.Contains(feedback, "TypeError") {
			t.Fatalf("the exception did not survive the round trip: %q", feedback)
		}
	})

	t.Run("a screenshot is left where the control plane can find it", func(t *testing.T) {
		report, err := verifier.Verify(ctx, verify.Request{
			ForkID:     "fork-shot",
			PreviewURL: server.URL + "/healthy",
		})
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		var shot string
		for _, check := range report.Checks {
			if check.Screenshot != "" {
				shot = check.Screenshot
			}
		}
		if shot == "" {
			t.Fatal("no check carried a screenshot")
		}
		info, err := os.Stat(shot)
		if err != nil || info.Size() == 0 {
			t.Fatalf("the reported screenshot is not a usable file: %v", err)
		}
	})
}

// TestAVMWithNoBrowserReportsThatPlainly guards the deployment that is built
// without one: the fix loop must be told the harness could not run, not handed
// a pass or an unexplained empty report.
func TestAVMWithNoBrowserReportsThatPlainly(t *testing.T) {
	// No t.Parallel: this sets an environment variable for the process.
	server := previewServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Setenv(browser.BinaryEnv, filepath.Join(t.TempDir(), "no-such-browser"))
	got := run(ctx, job{ForkID: "fork-4", PreviewURL: server.URL + "/healthy"}, settings{})
	if got.Passed {
		t.Fatal("a verification with no browser reported a pass")
	}
	if !strings.Contains(got.Checks[len(got.Checks)-1].Detail, "could not start a browser") {
		t.Fatalf("unhelpful detail: %+v", got.Checks)
	}
}
