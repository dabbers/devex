package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/browser"
)

// reachTimeout bounds the plain HTTP request that checks the preview answers
// at all, before a browser is worth starting.
const reachTimeout = 20 * time.Second

// run performs one verification and returns its report.
func run(ctx context.Context, request job, settings settings) report {
	reach := reachable(ctx, request.PreviewURL)
	if !reach.Passed {
		// Nothing a browser could add: the preview is not answering. Saying so
		// in one check beats a page of consequential failures.
		return assemble(request, []check{reach})
	}

	instance, err := browser.Launch(ctx, browser.Options{})
	if err != nil {
		return assemble(request, []check{reach, {
			Name:   "browser",
			Detail: "the harness could not start a browser: " + err.Error(),
		}})
	}
	defer func() {
		// A browser that will not shut down leaves a process and a profile
		// directory behind on a machine that serves every verification, so it
		// is said out loud rather than dropped.
		if err := instance.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "dabberz-verify: closing the browser:", err)
		}
	}()

	seen, err := browser.Visit(ctx, instance.Client(), request.PreviewURL, settings.settle)
	if err != nil {
		return assemble(request, []check{reach, {
			Name:   "browser",
			Detail: "the browser could not be driven: " + err.Error(),
		}})
	}

	checks := append([]check{reach}, observedChecks(seen)...)
	if path := saveScreenshot(settings.artifacts, request, seen.Screenshot); path != "" {
		// The screenshot is evidence for every check, so it hangs off the one
		// about what the page rendered.
		for i := range checks {
			if checks[i].Name == "renders" {
				checks[i].Screenshot = path
			}
		}
	}
	return assemble(request, checks)
}

// reachable checks the preview answers over the public network.
func reachable(ctx context.Context, previewURL string) check {
	ctx, cancel := context.WithTimeout(ctx, reachTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, previewURL, nil)
	if err != nil {
		return check{Name: "reachable", Detail: fmt.Sprintf("%s is not a usable URL: %v", previewURL, err)}
	}
	// A preview is served to people, and some stacks answer a bare client
	// differently; asking as a browser keeps this check measuring the same
	// thing the browser will see.
	req.Header.Set("User-Agent", "dabberz-verify/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return check{
			Name:   "reachable",
			Detail: fmt.Sprintf("%s did not answer: %v", previewURL, err),
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return check{
			Name:   "reachable",
			Detail: fmt.Sprintf("%s answered %s", previewURL, resp.Status),
		}
	}
	return check{
		Name:   "reachable",
		Passed: true,
		Detail: fmt.Sprintf("%s answered %s", previewURL, resp.Status),
	}
}

// observedChecks turns what the browser saw into the verdicts the fix loop
// reads. It is deliberately separate from the driving: what counts as a
// failure is a judgement, and this is where it is all written down.
func observedChecks(seen *browser.Observations) []check {
	loads := check{Name: "loads"}
	switch {
	case seen.NavigationError != "":
		loads.Detail = "the browser could not navigate: " + seen.NavigationError
	case seen.DocumentStatus >= 400:
		loads.Detail = fmt.Sprintf("the page returned HTTP %d", seen.DocumentStatus)
	case !seen.Loaded:
		loads.Detail = "the page never finished loading"
	default:
		loads.Passed = true
		loads.Detail = fmt.Sprintf("HTTP %d, title %q", seen.DocumentStatus, seen.Title)
	}

	// A page that loads but paints nothing is the most common thing a coding
	// agent breaks, and an HTTP 200 hides it completely.
	renders := check{Name: "renders"}
	if seen.TextLength == 0 && seen.VisualCount == 0 && seen.ElementCount < 3 {
		renders.Detail = fmt.Sprintf(
			"the page rendered nothing a user could see (%d elements, no text, no images or canvas)",
			seen.ElementCount)
	} else {
		renders.Passed = true
		renders.Detail = fmt.Sprintf("%d characters of text across %d elements",
			seen.TextLength, seen.ElementCount)
	}

	errorsCheck := check{Name: "no_page_errors", Passed: len(seen.Exceptions) == 0}
	if !errorsCheck.Passed {
		errorsCheck.Detail = "uncaught exceptions on the page:\n  " +
			strings.Join(dedupe(seen.Exceptions), "\n  ")
	}

	resources := check{Name: "resources", Passed: len(seen.FailedRequests) == 0}
	if !resources.Passed {
		failures := make([]string, 0, len(seen.FailedRequests))
		for _, failure := range seen.FailedRequests {
			failures = append(failures, failure.String())
		}
		resources.Detail = "the page asked for things it did not get:\n  " +
			strings.Join(dedupe(failures), "\n  ")
	}

	console := check{Name: "console", Passed: len(seen.ConsoleErrors) == 0}
	if !console.Passed {
		console.Detail = "console errors:\n  " + strings.Join(dedupe(seen.ConsoleErrors), "\n  ")
	}

	return []check{loads, renders, errorsCheck, resources, console}
}

// dedupe keeps the first of each repeated line. A loop that fails on every
// frame produces the same message hundreds of times, and a report is feedback
// to read, not a log.
func dedupe(lines []string) []string {
	seen := make(map[string]bool, len(lines))
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if seen[line] {
			continue
		}
		seen[line] = true
		kept = append(kept, line)
	}
	return kept
}

// assemble builds the report from its checks.
func assemble(request job, checks []check) report {
	failed := 0
	for _, one := range checks {
		if !one.Passed {
			failed++
		}
	}

	built := report{Passed: failed == 0, Checks: checks}
	if built.Passed {
		built.Summary = fmt.Sprintf("The preview at %s loaded and rendered with no errors.", request.PreviewURL)
	} else {
		built.Summary = fmt.Sprintf("The preview at %s failed %d of %d checks.",
			request.PreviewURL, failed, len(checks))
	}
	if intent := strings.TrimSpace(request.Intent); intent != "" {
		// Said plainly because it is a real limit: the harness checks that the
		// preview works, not that it does what the workstream set out to do.
		built.Summary += "\nWhat the work was meant to achieve was not checked automatically: " + intent
	}
	return built
}

// saveScreenshot writes the screenshot where the control plane can find it and
// returns its path, or "" if there was nothing to save or nowhere to put it.
func saveScreenshot(dir string, request job, png []byte) string {
	if len(png) == 0 {
		return ""
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "dabberz-verify")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "dabberz-verify: screenshot directory:", err)
		return ""
	}

	name := fmt.Sprintf("%s-%s-%d.png",
		safeName(request.ForkID), safeName(request.Profile), time.Now().UnixNano())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, png, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "dabberz-verify: write screenshot:", err)
		return ""
	}
	return path
}

// safeName reduces an identifier to something that cannot escape the artifact
// directory or surprise a shell reading the report.
func safeName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, name)
	if cleaned == "" {
		return "unknown"
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}
