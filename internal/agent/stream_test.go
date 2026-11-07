package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/vm"
)

// streamingDriver replays scripted stream-json lines through the streaming
// capability, and records whether streaming was used at all.
type streamingDriver struct {
	lines    []string
	result   *vm.ExecResult
	streamed bool
	lastCmd  vm.Command
}

func (d *streamingDriver) Name() string                                  { return "streaming" }
func (d *streamingDriver) Capacity(context.Context) (vm.Capacity, error) { return vm.Capacity{}, nil }
func (d *streamingDriver) Create(context.Context, vm.Spec) (*vm.Instance, error) {
	return nil, vm.ErrNotSupported
}
func (d *streamingDriver) Get(context.Context, string) (*vm.Instance, error) {
	return nil, vm.ErrNotFound
}
func (d *streamingDriver) List(context.Context) ([]*vm.Instance, error) { return nil, nil }
func (d *streamingDriver) Destroy(context.Context, string) error        { return nil }

func (d *streamingDriver) Exec(_ context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	d.lastCmd = cmd
	return d.result, nil
}

func (d *streamingDriver) ExecStream(_ context.Context, _ string, cmd vm.Command, onLine func([]byte)) (*vm.ExecResult, error) {
	d.streamed = true
	d.lastCmd = cmd
	for _, line := range d.lines {
		onLine([]byte(line))
	}
	return d.result, nil
}

func deltaLine(text string) string {
	body, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type":  "content_block_delta",
			"delta": map[string]any{"type": "text_delta", "text": text},
		},
	})
	return string(body)
}

func assistantLine(text string) string {
	body, _ := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}},
	})
	return string(body)
}

func TestStreamingDeliversTextAsItIsWritten(t *testing.T) {
	driver := &streamingDriver{
		lines: []string{
			deltaLine("Looking at "),
			deltaLine("the cart page"),
			deltaLine(". Done."),
		},
		result: &vm.ExecResult{Stdout: claudeJSON("Looking at the cart page. Done.", false, 100, 20, 0.05)},
	}
	r := New(driver, Config{})

	var seen []string
	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "fix it",
		OnText: func(text string) { seen = append(seen, text) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !driver.streamed {
		t.Fatal("the run did not use the streaming capability")
	}
	if len(seen) != 3 {
		t.Fatalf("got %d updates, want one per delta: %v", len(seen), seen)
	}
	// Each call carries everything written so far, so a consumer that renders
	// it does not accumulate and one that misses a call has no hole.
	if seen[0] != "Looking at " || seen[2] != "Looking at the cart page. Done." {
		t.Fatalf("updates are not cumulative: %v", seen)
	}
	// The result still comes from the final object, so one definition of what
	// a run cost rather than two.
	if res.Usage.CostUSD != 0.05 {
		t.Fatalf("cost = %v, want the result object's 0.05", res.Usage.CostUSD)
	}
	if res.Output != "Looking at the cart page. Done." {
		t.Fatalf("output = %q", res.Output)
	}
}

// TestAFinishedBlockReplacesItsDeltas: partial messages give deltas while the
// agent writes, and the assistant line then repeats the same block in full.
// Appending both would show every sentence twice.
func TestAFinishedBlockReplacesItsDeltas(t *testing.T) {
	driver := &streamingDriver{
		lines: []string{
			deltaLine("Half a "),
			deltaLine("sentence"),
			assistantLine("Half a sentence, finished."),
		},
		result: &vm.ExecResult{Stdout: claudeJSON("Half a sentence, finished.", false, 10, 5, 0.01)},
	}
	r := New(driver, Config{})

	var last string
	if _, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "go",
		OnText: func(text string) { last = text },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if last != "Half a sentence, finished." {
		t.Fatalf("text = %q, want the finished block alone", last)
	}
	if strings.Count(last, "Half a ") != 1 {
		t.Fatalf("the deltas were shown twice: %q", last)
	}
}

func TestStreamingIsOnlyAskedForWhenSomethingIsWatching(t *testing.T) {
	driver := &streamingDriver{
		result: &vm.ExecResult{Stdout: claudeJSON("done", false, 10, 5, 0.01)},
	}
	r := New(driver, Config{})

	// No OnText: a few thousand progress lines nobody reads is worse than one
	// result object.
	if _, err := r.Run(context.Background(), Request{InstanceID: "vm_1", Prompt: "go"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if driver.streamed {
		t.Fatal("streaming was used with no consumer")
	}
	if !strings.Contains(strings.Join(driver.lastCmd.Argv, " "), "--output-format json") {
		t.Fatalf("argv = %v, want print mode", driver.lastCmd.Argv)
	}
}

func TestANonStreamingDriverStillWorks(t *testing.T) {
	// fakeDriver does not implement vm.Streaming, so a caller asking for text
	// must still get a working run rather than an error.
	driver := &fakeDriver{result: &vm.ExecResult{Stdout: claudeJSON("done", false, 10, 5, 0.02)}}
	r := New(driver, Config{})

	called := false
	res, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "go",
		OnText: func(string) { called = true },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatal("a driver that cannot stream produced streamed text")
	}
	if res.Output != "done" || res.Usage.CostUSD != 0.02 {
		t.Fatalf("result = %+v", res)
	}
}

func TestStreamingArgvAsksForPartialMessages(t *testing.T) {
	driver := &streamingDriver{
		result: &vm.ExecResult{Stdout: claudeJSON("done", false, 10, 5, 0.01)},
	}
	r := New(driver, Config{})

	if _, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "go", OnText: func(string) {},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv := strings.Join(driver.lastCmd.Argv, " ")
	for _, want := range []string{"--output-format stream-json", "--verbose", "--include-partial-messages"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv %q is missing %q", argv, want)
		}
	}
}

func TestNoiseInTheStreamIsIgnored(t *testing.T) {
	driver := &streamingDriver{
		lines: []string{
			"not json at all",
			`{"type":"system","subtype":"init"}`,
			deltaLine("real text"),
			`{"type":"stream_event","event":{"type":"content_block_stop"}}`,
		},
		result: &vm.ExecResult{Stdout: claudeJSON("real text", false, 10, 5, 0.01)},
	}
	r := New(driver, Config{})

	var updates []string
	if _, err := r.Run(context.Background(), Request{
		InstanceID: "vm_1", Prompt: "go",
		OnText: func(text string) { updates = append(updates, text) },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only the line that was the agent writing counts as a change.
	if len(updates) != 1 || updates[0] != "real text" {
		t.Fatalf("updates = %v, want exactly the one text delta", updates)
	}
}
