// Package llm talks to the orchestrator's model over an OpenAI-shaped API.
//
// The orchestrator model is pluggable on purpose: it sits outside Claude
// Code's reasoning loop and does planning, fork/spawn/kill decisions and
// overlap judgement, none of which need the coding model. Anything speaking
// the OpenAI chat-completions shape works, DeepSeek included, so switching
// providers is a base URL and a model name.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Defaults for a fresh deployment.
const (
	DefaultBaseURL    = "https://api.deepseek.com/v1"
	DefaultModel      = "deepseek-chat"
	DefaultTimeout    = 120 * time.Second
	DefaultMaxRetries = 3
)

// Role names in a conversation.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// ErrNoChoices reports a response that carried no completion.
var ErrNoChoices = errors.New("llm: response contained no choices")

// Message is one turn in a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// System, User and Assistant build messages.
func System(content string) Message    { return Message{Role: RoleSystem, Content: content} }
func User(content string) Message      { return Message{Role: RoleUser, Content: content} }
func Assistant(content string) Message { return Message{Role: RoleAssistant, Content: content} }

// Request is a completion request.
type Request struct {
	Messages []Message `json:"messages"`
	// Model overrides the client's configured model.
	Model string `json:"model,omitempty"`
	// Temperature is passed through when non-nil, so a caller can ask for
	// determinism without every caller having to opt in.
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	// JSON asks the provider to emit a single JSON object. Planning output is
	// parsed, not read, so this is the normal mode.
	JSON bool `json:"-"`
}

// Usage is what a call cost.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Response is a completion.
type Response struct {
	Content string `json:"content"`
	Model   string `json:"model"`
	Usage   Usage  `json:"usage"`
	// FinishReason is the provider's reason for stopping, which distinguishes
	// a complete answer from one truncated at the token limit.
	FinishReason string `json:"finish_reason"`
}

// Truncated reports whether the provider stopped at the token limit rather
// than finishing its answer. Parsing a truncated JSON plan would fail in
// confusing ways, so callers check this first.
func (r *Response) Truncated() bool { return r.FinishReason == "length" }

// Client is the model interface the orchestrator depends on. Keeping it an
// interface is what lets tests run the whole planning path without a network.
type Client interface {
	// Complete returns a single completion.
	Complete(ctx context.Context, req Request) (*Response, error)
	// Model reports the model in use, for logging and cost attribution.
	Model() string
}

// Config configures an HTTP client.
type Config struct {
	// BaseURL is the API root, including any version segment.
	BaseURL string `yaml:"base_url" json:"base_url"`
	// APIKey authenticates to the provider.
	APIKey string `yaml:"api_key" json:"api_key"`
	// Model is the default model name.
	Model string `yaml:"model" json:"model"`
	// Timeout bounds a single call.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// MaxRetries bounds retries of transient failures.
	MaxRetries int `yaml:"max_retries" json:"max_retries"`
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	return c
}

// HTTPClient calls an OpenAI-shaped chat completions endpoint.
type HTTPClient struct {
	cfg  Config
	http *http.Client
	// sleep is swappable so tests do not wait out real backoff.
	sleep func(context.Context, time.Duration) error
}

// New returns a client for cfg.
func New(cfg Config, httpClient *http.Client) (*HTTPClient, error) {
	cfg = cfg.withDefaults()
	if cfg.APIKey == "" {
		return nil, errors.New("llm: an API key is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}
	return &HTTPClient{cfg: cfg, http: httpClient, sleep: sleepCtx}, nil
}

// Model implements Client.
func (c *HTTPClient) Model() string { return c.cfg.Model }

// chatRequest is the wire format sent to the provider.
type chatRequest struct {
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	Temperature    *float64  `json:"temperature,omitempty"`
	MaxTokens      int       `json:"max_tokens,omitempty"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

// chatResponse is the wire format returned by the provider.
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete implements Client, retrying transient failures with backoff.
func (c *HTTPClient) Complete(ctx context.Context, req Request) (*Response, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("llm: a request needs at least one message")
	}

	model := req.Model
	if model == "" {
		model = c.cfg.Model
	}
	wire := chatRequest{
		Model:       model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if req.JSON {
		wire.ResponseFormat = &struct {
			Type string `json:"type"`
		}{Type: "json_object"}
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}

		resp, err := c.attempt(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !Retryable(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("llm: giving up after %d attempts: %w", c.cfg.MaxRetries+1, lastErr)
}

// attempt performs a single HTTP call.
func (c *HTTPClient) attempt(ctx context.Context, body []byte) (*Response, error) {
	endpoint := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure is worth another try: the provider may simply
		// have dropped the connection.
		return nil, &Error{Status: 0, Message: err.Error(), transient: true}
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 8<<20))
	if err != nil {
		return nil, &Error{Status: httpResp.StatusCode, Message: err.Error(), transient: true}
	}

	if httpResp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(raw))
		var decoded chatResponse
		if json.Unmarshal(raw, &decoded) == nil && decoded.Error != nil {
			message = decoded.Error.Message
		}
		return nil, &Error{
			Status:  httpResp.StatusCode,
			Message: message,
			// Rate limits and server errors are worth retrying; a bad request
			// or a rejected key will fail identically every time.
			transient: httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500,
			RateLimit: httpResp.StatusCode == http.StatusTooManyRequests,
		}
	}

	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return nil, ErrNoChoices
	}

	return &Response{
		Content:      decoded.Choices[0].Message.Content,
		Model:        decoded.Model,
		FinishReason: decoded.Choices[0].FinishReason,
		Usage: Usage{
			InputTokens:  decoded.Usage.PromptTokens,
			OutputTokens: decoded.Usage.CompletionTokens,
		},
	}, nil
}

// Error is a provider-level failure.
type Error struct {
	// Status is the HTTP status, or 0 for a transport failure.
	Status int
	// Message is the provider's explanation.
	Message string
	// RateLimit marks a 429, which the orchestrator surfaces rather than
	// treating as a generic failure.
	RateLimit bool
	transient bool
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "llm: request failed: " + e.Message
	}
	return fmt.Sprintf("llm: provider returned %d: %s", e.Status, e.Message)
}

// Retryable reports whether an error is worth another attempt.
func Retryable(err error) bool {
	var providerErr *Error
	if errors.As(err, &providerErr) {
		return providerErr.transient
	}
	return false
}

// IsRateLimit reports whether an error is an upstream rate limit. Rate limits
// are folded into the existing tripwire and pause-and-retry handling rather
// than being capped for in advance.
func IsRateLimit(err error) bool {
	var providerErr *Error
	return errors.As(err, &providerErr) && providerErr.RateLimit
}

// backoff returns the delay before the nth retry, growing exponentially.
func backoff(attempt int) time.Duration {
	const base = 500 * time.Millisecond
	const cap = 30 * time.Second
	delay := time.Duration(math.Pow(2, float64(attempt-1))) * base
	return min(delay, cap)
}

// sleepCtx waits for d unless ctx finishes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DecodeJSON parses a JSON response body into dst, tolerating the fenced code
// blocks models sometimes wrap JSON in even when asked not to.
func DecodeJSON(content string, dst any) error {
	cleaned := strings.TrimSpace(content)
	if fenced, ok := strings.CutPrefix(cleaned, "```"); ok {
		// Drop the opening fence's language tag and the closing fence.
		if _, rest, ok := strings.Cut(fenced, "\n"); ok {
			cleaned = rest
		}
		if idx := strings.LastIndex(cleaned, "```"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	if err := json.Unmarshal([]byte(cleaned), dst); err != nil {
		return fmt.Errorf("llm: model did not return usable JSON: %w", err)
	}
	return nil
}

var _ Client = (*HTTPClient)(nil)
