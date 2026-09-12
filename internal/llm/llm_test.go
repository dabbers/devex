package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a client at a test server and removes retry delays.
func newTestClient(t *testing.T, srv *httptest.Server, cfg Config) *HTTPClient {
	t.Helper()
	cfg.BaseURL = srv.URL
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	c, err := New(cfg, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func okResponse(content string) string {
	return `{
		"model": "deepseek-chat",
		"choices": [{"message": {"role": "assistant", "content": ` + strconv(content) + `}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 120, "completion_tokens": 45}
	}`
}

func strconv(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestNewRequiresAnAPIKey(t *testing.T) {
	if _, err := New(Config{}, nil); err == nil {
		t.Error("New should require an API key")
	}
}

func TestCompleteSendsAnOpenAIShapedRequest(t *testing.T) {
	var captured struct {
		path string
		auth string
		body []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.body, _ = io.ReadAll(r.Body)
		//nolint:errcheck // test server
		io.WriteString(w, okResponse("hello"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Config{Model: "deepseek-chat"})
	resp, err := c.Complete(context.Background(), Request{
		Messages: []Message{System("plan carefully"), User("add ratings")},
		JSON:     true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if captured.path != "/chat/completions" {
		t.Fatalf("posted to %q", captured.path)
	}
	if captured.auth != "Bearer test-key" {
		t.Fatalf("authorization = %q", captured.auth)
	}

	var sent chatRequest
	if err := json.Unmarshal(captured.body, &sent); err != nil {
		t.Fatalf("request body is not valid JSON: %v", err)
	}
	if sent.Model != "deepseek-chat" || len(sent.Messages) != 2 {
		t.Fatalf("unexpected request: %+v", sent)
	}
	if sent.ResponseFormat == nil || sent.ResponseFormat.Type != "json_object" {
		t.Fatal("JSON mode was not requested")
	}

	if resp.Content != "hello" {
		t.Fatalf("content = %q", resp.Content)
	}
	// Usage feeds the cost half of the tripwire, so it must be carried through.
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 45 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestCompleteRequiresMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if _, err := newTestClient(t, srv, Config{}).Complete(context.Background(), Request{}); err == nil {
		t.Error("Complete should reject an empty conversation")
	}
}

func TestCompleteRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			//nolint:errcheck // test server
			io.WriteString(w, `{"error":{"message":"upstream hiccup"}}`)
			return
		}
		//nolint:errcheck // test server
		io.WriteString(w, okResponse("recovered"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Config{MaxRetries: 3})
	resp, err := c.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "recovered" {
		t.Fatalf("content = %q", resp.Content)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("made %d calls, want 3", got)
	}
}

func TestCompleteDoesNotRetryPermanentFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		//nolint:errcheck // test server
		io.WriteString(w, `{"error":{"message":"unknown model"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Config{MaxRetries: 3})
	_, err := c.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("expected a 400 to fail")
	}
	// Retrying a rejected request just burns time and quota.
	if got := calls.Load(); got != 1 {
		t.Fatalf("made %d calls, want 1", got)
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error lost the provider's explanation: %v", err)
	}
	if Retryable(err) {
		t.Error("a 400 should not be classed as retryable")
	}
}

func TestRateLimitsAreIdentifiable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		//nolint:errcheck // test server
		io.WriteString(w, `{"error":{"message":"rate limit reached"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Config{MaxRetries: 1})
	_, err := c.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("expected a rate limit to surface as an error")
	}
	// The orchestrator folds rate limits into its pause-and-retry handling, so
	// it has to be able to tell one apart from a generic failure.
	if !IsRateLimit(err) {
		t.Fatalf("IsRateLimit = false for %v", err)
	}
}

func TestCompleteReportsMissingChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		//nolint:errcheck // test server
		io.WriteString(w, `{"model":"m","choices":[]}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, Config{}).Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if !errors.Is(err, ErrNoChoices) {
		t.Fatalf("Complete = %v, want ErrNoChoices", err)
	}
}

func TestCompleteHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Config{MaxRetries: 5})
	// Restore real sleeping so cancellation has something to interrupt.
	c.sleep = sleepCtx

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Complete(ctx, Request{Messages: []Message{User("hi")}}); err == nil {
		t.Fatal("expected cancellation to abort the retry loop")
	}
}

func TestTruncatedResponsesAreFlagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		//nolint:errcheck // test server
		io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"{\"partial\":"},"finish_reason":"length"}]}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv, Config{}).Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// A plan cut off at the token limit would fail to parse in a confusing
	// way, so callers need to be able to detect it directly.
	if !resp.Truncated() {
		t.Fatal("a response finished for length should report as truncated")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	if backoff(1) >= backoff(2) {
		t.Error("backoff should grow with attempts")
	}
	if got := backoff(50); got > 30*time.Second {
		t.Errorf("backoff(50) = %v, want it capped", got)
	}
}

func TestDecodeJSON(t *testing.T) {
	type plan struct {
		Summary string `json:"summary"`
	}

	for name, content := range map[string]string{
		"plain":        `{"summary":"two workstreams"}`,
		"whitespace":   "  \n{\"summary\":\"two workstreams\"}\n ",
		"fenced":       "```json\n{\"summary\":\"two workstreams\"}\n```",
		"fenced plain": "```\n{\"summary\":\"two workstreams\"}\n```",
	} {
		var got plan
		if err := DecodeJSON(content, &got); err != nil {
			t.Errorf("DecodeJSON(%s) = %v", name, err)
			continue
		}
		if got.Summary != "two workstreams" {
			t.Errorf("DecodeJSON(%s) = %+v", name, got)
		}
	}

	var got plan
	if err := DecodeJSON("not json at all", &got); err == nil {
		t.Error("DecodeJSON should reject non-JSON output")
	}
}

func TestMockReplaysResponses(t *testing.T) {
	m := NewMock(Response{Content: "first"}, Response{Content: "second"})
	ctx := context.Background()

	for _, want := range []string{"first", "second", "second"} {
		resp, err := m.Complete(ctx, Request{Messages: []Message{User("hi")}})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Content != want {
			t.Fatalf("content = %q, want %q", resp.Content, want)
		}
	}
	if m.Calls() != 3 {
		t.Fatalf("Calls = %d, want 3", m.Calls())
	}
	if m.LastRequest().Messages[0].Content != "hi" {
		t.Fatal("mock did not record the request")
	}

	failing := &Mock{Err: errors.New("boom")}
	if _, err := failing.Complete(ctx, Request{Messages: []Message{User("hi")}}); err == nil {
		t.Error("a mock with an error should return it")
	}
	empty := NewMock()
	if _, err := empty.Complete(ctx, Request{Messages: []Message{User("hi")}}); err == nil {
		t.Error("a mock with no scripted responses should report that")
	}
}
