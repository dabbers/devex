package verify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/vm"
)

// scriptedDriver returns a canned result and records concurrency.
type scriptedDriver struct {
	mu        sync.Mutex
	result    *vm.ExecResult
	err       error
	delay     time.Duration
	inFlight  int
	peak      int
	calls     int
	profiles  []string
	lastStdin []byte
}

func (d *scriptedDriver) Name() string                                  { return "scripted" }
func (d *scriptedDriver) Capacity(context.Context) (vm.Capacity, error) { return vm.Capacity{}, nil }
func (d *scriptedDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (d *scriptedDriver) Get(context.Context, string) (*vm.Instance, error) {
	return nil, vm.ErrNotFound
}
func (d *scriptedDriver) List(context.Context) ([]*vm.Instance, error) { return nil, nil }
func (d *scriptedDriver) Destroy(context.Context, string) error        { return nil }

func (d *scriptedDriver) Exec(_ context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	d.mu.Lock()
	d.calls++
	d.inFlight++
	d.peak = max(d.peak, d.inFlight)
	d.lastStdin = cmd.Stdin
	var job Request
	//nolint:errcheck // the job is always JSON here
	json.Unmarshal(cmd.Stdin, &job)
	d.profiles = append(d.profiles, job.Profile)
	err := d.err
	result := d.result
	delay := d.delay
	d.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	d.mu.Lock()
	d.inFlight--
	d.mu.Unlock()
	return result, err
}

func reportJSON(passed bool, summary string, checks []Check) string {
	body, _ := json.Marshal(map[string]any{"passed": passed, "summary": summary, "checks": checks})
	return string(body)
}

func newVerifier(t *testing.T, driver vm.Driver, profiles int) *Verifier {
	t.Helper()
	v, err := New(driver, Config{UIInstanceID: "vm_ui", Profiles: profiles})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestNewRequiresTheSharedUIVM(t *testing.T) {
	if _, err := New(&scriptedDriver{}, Config{}); err == nil {
		t.Error("a verifier without a UI VM should be rejected")
	}
}

func TestVerifyRunsOnTheSharedUIVMAgainstThePublicURL(t *testing.T) {
	driver := &scriptedDriver{result: &vm.ExecResult{Stdout: reportJSON(true, "all good", nil)}}
	v := newVerifier(t, driver, 2)

	report, err := v.Verify(context.Background(), Request{
		ForkID:     "fork_1",
		PreviewURL: "https://preview-web-ratings.dab.im",
		Intent:     "star ratings render and submit",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed || report.Summary != "all good" {
		t.Fatalf("report = %+v", report)
	}
	if report.Profile == "" {
		t.Fatal("the report should record which profile ran the job")
	}

	var job Request
	if err := json.Unmarshal(driver.lastStdin, &job); err != nil {
		t.Fatalf("the job was not sent as JSON: %v", err)
	}
	// The verifier must go through the public URL, not a co-located shortcut,
	// so routing and TLS are exercised the way a user would exercise them.
	if job.PreviewURL != "https://preview-web-ratings.dab.im" {
		t.Fatalf("job URL = %q", job.PreviewURL)
	}
	if job.Intent == "" {
		t.Fatal("the workstream's intent should reach the verifier")
	}
}

func TestVerifyRequiresAPreviewURL(t *testing.T) {
	v := newVerifier(t, &scriptedDriver{result: &vm.ExecResult{}}, 2)
	if _, err := v.Verify(context.Background(), Request{ForkID: "fork_1"}); err == nil {
		t.Error("verification without a preview URL should be rejected")
	}
}

func TestFailingVerificationIsAReportNotAnError(t *testing.T) {
	driver := &scriptedDriver{result: &vm.ExecResult{
		Stdout: reportJSON(false, "the star widget does not render", []Check{
			{Name: "ratings visible", Passed: false, Detail: "no element matched .rating"},
			{Name: "page loads", Passed: true},
		}),
	}}

	// The fix loop reads this and feeds it back, so it must not arrive as an error.
	report, err := newVerifier(t, driver, 2).Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
	if err != nil {
		t.Fatalf("Verify returned an error for a failed check: %v", err)
	}
	if report.Passed {
		t.Fatal("the report should not have passed")
	}

	feedback := report.FeedbackText()
	if !strings.Contains(feedback, "star widget does not render") {
		t.Fatalf("feedback lost the summary: %q", feedback)
	}
	if !strings.Contains(feedback, "ratings visible") {
		t.Fatalf("feedback lost the failing check: %q", feedback)
	}
	// Passing checks are noise in a fix prompt.
	if strings.Contains(feedback, "page loads") {
		t.Fatalf("feedback included a passing check: %q", feedback)
	}
}

func TestUnusableOutputFailsRatherThanPassingByDefault(t *testing.T) {
	driver := &scriptedDriver{result: &vm.ExecResult{
		Stdout:   "Traceback: browser crashed",
		Stderr:   "chromium exited unexpectedly",
		ExitCode: 1,
	}}
	report, err := newVerifier(t, driver, 2).Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// A harness that verified nothing must never be read as a pass.
	if report.Passed {
		t.Fatal("an unusable verifier report must not count as a pass")
	}
	if !strings.Contains(report.Summary, "chromium exited") {
		t.Fatalf("summary should carry the harness failure: %q", report.Summary)
	}
}

func TestTimeoutIsAFailedVerification(t *testing.T) {
	driver := &scriptedDriver{result: &vm.ExecResult{TimedOut: true}}
	report, err := newVerifier(t, driver, 2).Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Passed || !strings.Contains(report.Summary, "did not finish") {
		t.Fatalf("report = %+v", report)
	}
}

func TestReportIsParsedAmongHarnessLogging(t *testing.T) {
	stdout := "starting chromium\nnavigating\n" + reportJSON(true, "ok", nil)
	driver := &scriptedDriver{result: &vm.ExecResult{Stdout: stdout}}
	report, err := newVerifier(t, driver, 2).Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed {
		t.Fatalf("report = %+v", report)
	}
}

func TestConcurrencyIsCappedByTheUIVMProfileCount(t *testing.T) {
	const profiles = 3
	driver := &scriptedDriver{
		result: &vm.ExecResult{Stdout: reportJSON(true, "ok", nil)},
		delay:  20 * time.Millisecond,
	}
	v := newVerifier(t, driver, profiles)

	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"}); err != nil {
				t.Errorf("Verify: %v", err)
			}
		}()
	}
	wg.Wait()

	driver.mu.Lock()
	defer driver.mu.Unlock()
	// One browser with N profiles: more than N concurrent jobs would mean
	// starting browsers the UI VM cannot afford.
	if driver.peak > profiles {
		t.Fatalf("peak concurrency was %d, above the %d profiles available", driver.peak, profiles)
	}
	if driver.calls != 12 {
		t.Fatalf("ran %d jobs, want all 12 to have eventually run", driver.calls)
	}
}

func TestProfilesAreDistinctWhileInUseAndReused(t *testing.T) {
	const profiles = 2
	driver := &scriptedDriver{
		result: &vm.ExecResult{Stdout: reportJSON(true, "ok", nil)},
		delay:  20 * time.Millisecond,
	}
	v := newVerifier(t, driver, profiles)

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			//nolint:errcheck // asserted via the driver's records
			v.Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
		}()
	}
	wg.Wait()

	driver.mu.Lock()
	defer driver.mu.Unlock()
	distinct := map[string]bool{}
	for _, profile := range driver.profiles {
		if profile == "" {
			t.Fatal("a job ran without a profile assigned")
		}
		distinct[profile] = true
	}
	// Profiles are a fixed pool, handed out and returned rather than created
	// per job.
	if len(distinct) > profiles {
		t.Fatalf("saw %d distinct profiles, want at most %d", len(distinct), profiles)
	}
}

func TestQueuedJobsAreVisibleInStats(t *testing.T) {
	driver := &scriptedDriver{
		result: &vm.ExecResult{Stdout: reportJSON(true, "ok", nil)},
		delay:  200 * time.Millisecond,
	}
	v := newVerifier(t, driver, 1)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			//nolint:errcheck // asserted via stats
			v.Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
		}()
	}

	// The UI VM is full, so the extra jobs must be queued and visible as such.
	deadline := time.Now().Add(2 * time.Second)
	var sawQueue bool
	for time.Now().Before(deadline) {
		if v.Stats().Waiting > 0 {
			sawQueue = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()

	if !sawQueue {
		t.Fatal("jobs beyond the profile count should queue and be reported as waiting")
	}
	if stats := v.Stats(); stats.Held != 0 || stats.Waiting != 0 {
		t.Fatalf("the queue did not drain: %+v", stats)
	}
}

func TestVerifyReportsDriverFailures(t *testing.T) {
	driver := &scriptedDriver{err: vm.ErrNotFound}
	// Not being able to reach the UI VM is a dabberz problem, not a verdict.
	if _, err := newVerifier(t, driver, 2).Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"}); err == nil {
		t.Fatal("an unreachable UI VM should surface as an error")
	}
}

func TestVerifyHonoursCancellationWhileQueued(t *testing.T) {
	driver := &scriptedDriver{
		result: &vm.ExecResult{Stdout: reportJSON(true, "ok", nil)},
		delay:  500 * time.Millisecond,
	}
	v := newVerifier(t, driver, 1)

	go func() {
		//nolint:errcheck // occupies the only profile
		v.Verify(context.Background(), Request{ForkID: "f", PreviewURL: "https://x"})
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := v.Verify(ctx, Request{ForkID: "f2", PreviewURL: "https://y"}); err == nil {
		t.Fatal("a cancelled job should stop waiting in the queue")
	}
}

func TestFeedbackTextAlwaysSaysSomething(t *testing.T) {
	empty := &Report{Passed: false}
	if empty.FeedbackText() == "" {
		t.Fatal("feedback must never be empty, or the fix prompt says nothing")
	}
}
