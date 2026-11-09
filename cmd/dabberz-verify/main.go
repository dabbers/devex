// Command dabberz-verify checks a fork's preview by driving a real browser at
// it, and reports what it found.
//
// It is the harness end of internal/verify: the control plane runs this inside
// the shared UI VM, hands it a JSON job on stdin, and reads a JSON report off
// stdout. The report is the verdict, so a failed verification exits zero -- the
// fix loop needs to read the reason and feed it back to the coding agent, and a
// non-zero exit with no report would tell it nothing.
//
// The preview is reached over the network by its public URL, the way a user
// would reach it, rather than by a shortcut from inside the machine. A preview
// that only answers locally is not a working preview, and that is exactly the
// kind of break this is here to catch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is set at build time.
var version = "dev"

// stdinLimit bounds the job read from stdin. A job is a handful of fields.
const stdinLimit = 1 << 20

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			fmt.Println("dabberz-verify", version)
			return
		case "help", "-h", "--help":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "dabberz-verify: unknown argument %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}

	// Signals end the run early rather than killing it: a half-finished run
	// still has a report to give, and the control plane is waiting for one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	settings := settingsFromEnv()
	report := verifyJob(ctx, os.Stdin, settings)

	encoded, err := json.Marshal(report)
	if err != nil {
		// Unreachable for this shape, but an unwritable report is the one
		// failure this command cannot report through a report.
		fmt.Fprintln(os.Stderr, "dabberz-verify: encode report:", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

const usage = `dabberz-verify drives a browser against a preview and reports what it found.

It reads a JSON job on stdin and writes a JSON report on stdout:

  {"fork_id":"...","preview_url":"https://...","intent":"...","profile":"..."}

Environment:
  DABBERZ_BROWSER             browser binary to run (default: the first found)
  DABBERZ_VERIFY_TIMEOUT      budget for one run (default 2m)
  DABBERZ_VERIFY_SETTLE       wait after load for late renders and errors (default 1.5s)
  DABBERZ_VERIFY_ARTIFACTS    directory for screenshots (default a temp directory)
  DISPLAY                     when set, the browser runs with a real window
`

// job is one verification request, matching verify.Request on the wire.
type job struct {
	ForkID     string `json:"fork_id"`
	PreviewURL string `json:"preview_url"`
	Intent     string `json:"intent"`
	Profile    string `json:"profile"`
}

// check is one thing the harness looked at, matching verify.Check.
type check struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	Detail     string `json:"detail,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

// report is the verdict, matching the wire shape verify.decodeReport reads.
type report struct {
	Passed  bool    `json:"passed"`
	Summary string  `json:"summary"`
	Checks  []check `json:"checks,omitempty"`
}

// verifyJob reads a job, runs it, and always returns a report.
func verifyJob(ctx context.Context, stdin io.Reader, settings settings) report {
	request, err := readJob(stdin)
	if err != nil {
		return report{Summary: "dabberz-verify could not read its job: " + err.Error()}
	}

	ctx, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()
	return run(ctx, request, settings)
}

// readJob decodes and validates the job on stdin.
func readJob(stdin io.Reader) (job, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, stdinLimit))
	if err != nil {
		return job{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return job{}, errors.New("no job on stdin")
	}

	var request job
	if err := json.Unmarshal(raw, &request); err != nil {
		return job{}, fmt.Errorf("unreadable job: %w", err)
	}
	if request.PreviewURL == "" {
		return job{}, errors.New("the job names no preview URL")
	}
	return request, nil
}

// settings are the knobs the UI VM's operator can turn.
type settings struct {
	timeout   time.Duration
	settle    time.Duration
	artifacts string
}

func settingsFromEnv() settings {
	return settings{
		timeout:   durationEnv("DABBERZ_VERIFY_TIMEOUT", 2*time.Minute),
		settle:    durationEnv("DABBERZ_VERIFY_SETTLE", 1500*time.Millisecond),
		artifacts: os.Getenv("DABBERZ_VERIFY_ARTIFACTS"),
	}
}

// durationEnv reads a duration, falling back rather than failing: a typo in a
// VM's environment should not stop verifications from running.
func durationEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		fmt.Fprintf(os.Stderr, "dabberz-verify: ignoring %s=%q, using %s\n", name, raw, fallback)
		return fallback
	}
	return parsed
}
