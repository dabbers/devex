package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/browser"
	"github.com/dabbers/devex/internal/verify"
)

func TestReadJobRejectsWhatItCannotRun(t *testing.T) {
	t.Parallel()

	for name, input := range map[string]string{
		"empty":            "",
		"whitespace":       "   \n",
		"not json":         "hello",
		"no preview URL":   `{"fork_id":"f1"}`,
		"truncated":        `{"fork_id":`,
		"wrong type":       `{"preview_url":42}`,
		"array not object": `[{"preview_url":"https://x"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readJob(strings.NewReader(input)); err == nil {
				t.Fatalf("readJob(%q) accepted a job it cannot run", input)
			}
		})
	}

	got, err := readJob(strings.NewReader(`{"fork_id":"f1","preview_url":"https://p","intent":"do a thing","profile":"verifier-0"}`))
	if err != nil {
		t.Fatalf("readJob: %v", err)
	}
	if got.ForkID != "f1" || got.PreviewURL != "https://p" || got.Intent != "do a thing" || got.Profile != "verifier-0" {
		t.Fatalf("readJob decoded %+v", got)
	}
}

func TestAMissingJobStillProducesAReport(t *testing.T) {
	t.Parallel()

	// The control plane reads a report or nothing; a harness that dies quietly
	// tells the fix loop nothing it can act on.
	got := verifyJob(context.Background(), strings.NewReader(""), settings{timeout: time.Second})
	if got.Passed {
		t.Fatal("a job that could not be read reported a pass")
	}
	if got.Summary == "" {
		t.Fatal("no summary to report")
	}
}

func TestObservedChecksReadTheBrowsersEvidence(t *testing.T) {
	t.Parallel()

	healthy := browser.Observations{
		Loaded: true, DocumentStatus: 200, Title: "Dabberz",
		TextLength: 400, ElementCount: 40,
	}

	tests := map[string]struct {
		seen   browser.Observations
		failed []string
	}{
		"a working page": {seen: healthy},
		"navigation refused": {
			seen:   browser.Observations{NavigationError: "net::ERR_CONNECTION_REFUSED"},
			failed: []string{"loads", "renders"},
		},
		"server error": {
			seen:   browser.Observations{Loaded: true, DocumentStatus: 500, TextLength: 20, ElementCount: 5},
			failed: []string{"loads"},
		},
		"never finished loading": {
			seen:   browser.Observations{DocumentStatus: 200, TextLength: 20, ElementCount: 5},
			failed: []string{"loads"},
		},
		"blank page": {
			seen:   browser.Observations{Loaded: true, DocumentStatus: 200},
			failed: []string{"renders"},
		},
		"a canvas app with no text still renders": {
			seen: browser.Observations{
				Loaded: true, DocumentStatus: 200, ElementCount: 1, VisualCount: 1,
			},
		},
		"uncaught exception": {
			seen: browser.Observations{
				Loaded: true, DocumentStatus: 200, TextLength: 10, ElementCount: 5,
				Exceptions: []string{"TypeError: x is not a function"},
			},
			failed: []string{"no_page_errors"},
		},
		"a subresource that did not load": {
			seen: browser.Observations{
				Loaded: true, DocumentStatus: 200, TextLength: 10, ElementCount: 5,
				FailedRequests: []browser.FailedRequest{{URL: "https://p/app.js", Kind: "Script", Status: 404}},
			},
			failed: []string{"resources"},
		},
		"console errors": {
			seen: browser.Observations{
				Loaded: true, DocumentStatus: 200, TextLength: 10, ElementCount: 5,
				ConsoleErrors: []string{"failed to fetch /api/items"},
			},
			failed: []string{"console"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var failed []string
			for _, one := range observedChecks(&test.seen) {
				if one.Passed {
					continue
				}
				failed = append(failed, one.Name)
				if one.Detail == "" {
					t.Errorf("check %q failed without saying why", one.Name)
				}
			}
			if strings.Join(failed, ",") != strings.Join(test.failed, ",") {
				t.Fatalf("failed checks = %v, want %v", failed, test.failed)
			}
		})
	}
}

func TestRepeatedErrorsAreReportedOnce(t *testing.T) {
	t.Parallel()

	seen := browser.Observations{
		Loaded: true, DocumentStatus: 200, TextLength: 10, ElementCount: 5,
		Exceptions: []string{"TypeError: boom", "TypeError: boom", "TypeError: boom"},
	}
	for _, one := range observedChecks(&seen) {
		if one.Name != "no_page_errors" {
			continue
		}
		if strings.Count(one.Detail, "TypeError: boom") != 1 {
			t.Fatalf("a repeated exception was reported more than once: %q", one.Detail)
		}
	}
}

func TestTheReportIsWhatTheControlPlaneReads(t *testing.T) {
	t.Parallel()

	// The harness and internal/verify agree on a wire shape and nothing else.
	// If a field name drifts on either side, a verification silently stops
	// reporting what it found, so the contract is asserted rather than assumed.
	built := assemble(
		job{PreviewURL: "https://preview.example", Intent: "add a logout button"},
		[]check{
			{Name: "reachable", Passed: true, Detail: "answered 200 OK"},
			{Name: "renders", Detail: "nothing rendered", Screenshot: "/tmp/shot.png"},
		},
	)
	encoded, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded verify.Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the control plane could not read the report: %v", err)
	}
	if decoded.Passed {
		t.Fatal("a report with a failed check decoded as a pass")
	}
	if len(decoded.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(decoded.Checks))
	}
	if decoded.Checks[1].Screenshot != "/tmp/shot.png" {
		t.Fatalf("screenshot did not survive the wire: %+v", decoded.Checks[1])
	}
	if !strings.Contains(decoded.Summary, "add a logout button") {
		t.Fatalf("the intent did not reach the report: %q", decoded.Summary)
	}

	feedback := decoded.FeedbackText()
	if !strings.Contains(feedback, "FAILED renders") || strings.Contains(feedback, "FAILED reachable") {
		t.Fatalf("feedback to the agent names the wrong checks: %q", feedback)
	}
}

func TestSafeNameCannotEscapeTheArtifactDirectory(t *testing.T) {
	t.Parallel()

	for _, hostile := range []string{"../../etc/passwd", "/etc/shadow", "..", "a/b", ""} {
		got := safeName(hostile)
		if strings.ContainsAny(got, `/\.`) || got == "" {
			t.Fatalf("safeName(%q) = %q", hostile, got)
		}
	}
}

// pages are the fixtures the end-to-end runs drive a real browser against.
var pages = map[string]string{
	"/healthy": `<!doctype html><html><head><title>Healthy</title></head>
<body><h1>It works</h1><p>Some real content for a user to read.</p></body></html>`,

	"/blank": `<!doctype html><html><head><title>Blank</title></head><body></body></html>`,

	"/throws": `<!doctype html><html><head><title>Throws</title></head>
<body><h1>Loaded</h1><script>setTimeout(function(){ null.boom(); }, 10);</script></body></html>`,

	"/missing-asset": `<!doctype html><html><head><title>Missing</title></head>
<body><h1>Loaded</h1><script src="/gone.js"></script></body></html>`,

	"/console-error": `<!doctype html><html><head><title>Console</title></head>
<body><h1>Loaded</h1><script>console.error("could not reach the API");</script></body></html>`,
}

func previewServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func requireBrowser(t *testing.T) {
	t.Helper()
	if _, err := browser.Find(""); err != nil {
		t.Skip("no browser on this machine:", err)
	}
}

func TestItDrivesARealBrowser(t *testing.T) {
	requireBrowser(t)

	server := previewServer(t)
	artifacts := t.TempDir()

	tests := map[string]struct {
		path   string
		failed []string
	}{
		"a working preview":       {path: "/healthy"},
		"a preview that is blank": {path: "/blank", failed: []string{"renders"}},
		"an uncaught exception":   {path: "/throws", failed: []string{"no_page_errors"}},
		// Chrome reports a failed subresource through the network domain, not
		// as a console error, so this is one finding rather than two.
		"a script that is missing": {path: "/missing-asset", failed: []string{"resources"}},
		"a console error":          {path: "/console-error", failed: []string{"console"}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			got := run(ctx, job{
				ForkID:     "fork-1",
				PreviewURL: server.URL + test.path,
				Profile:    "verifier-0",
			}, settings{settle: 750 * time.Millisecond, artifacts: artifacts})

			var failed []string
			for _, one := range got.Checks {
				if !one.Passed {
					failed = append(failed, one.Name)
				}
			}
			if strings.Join(failed, ",") != strings.Join(test.failed, ",") {
				t.Fatalf("failed checks = %v, want %v\nreport: %+v", failed, test.failed, got)
			}
			if got.Passed != (len(test.failed) == 0) {
				t.Fatalf("passed = %v with failures %v", got.Passed, failed)
			}

			// Every run photographs the page, including the broken ones: the
			// screenshot is how a person sees what the checks are describing.
			shots, _ := filepath.Glob(filepath.Join(artifacts, "fork-1-verifier-0-*.png"))
			if len(shots) == 0 {
				t.Fatal("no screenshot was saved")
			}
			info, err := os.Stat(shots[0])
			if err != nil || info.Size() == 0 {
				t.Fatalf("screenshot is unusable: %v", err)
			}
		})
	}
}

func TestAPreviewThatIsDownIsReportedWithoutABrowser(t *testing.T) {
	t.Parallel()

	server := previewServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := run(ctx, job{ForkID: "fork-2", PreviewURL: server.URL + "/broken"}, settings{})
	if got.Passed {
		t.Fatal("a preview answering 500 passed verification")
	}
	if len(got.Checks) != 1 || got.Checks[0].Name != "reachable" {
		t.Fatalf("checks = %+v, want only the reachability check", got.Checks)
	}
}

func TestAPreviewThatIsNotThereIsReported(t *testing.T) {
	t.Parallel()

	// Port 1 on the loopback interface has nothing listening on it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := run(ctx, job{ForkID: "fork-3", PreviewURL: "http://127.0.0.1:1/"}, settings{})
	if got.Passed {
		t.Fatal("a preview with nothing behind it passed verification")
	}
	if !strings.Contains(got.Checks[0].Detail, "did not answer") {
		t.Fatalf("unhelpful detail: %q", got.Checks[0].Detail)
	}
}
