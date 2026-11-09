package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FailedRequest is one subresource the page could not load.
type FailedRequest struct {
	URL    string
	Kind   string
	Status int
	Error  string
}

// String renders a failure the way it reads in feedback to an agent.
func (f FailedRequest) String() string {
	switch {
	case f.Error != "":
		return fmt.Sprintf("%s (%s): %s", f.URL, strings.ToLower(f.Kind), f.Error)
	case f.Status != 0:
		return fmt.Sprintf("%s (%s): HTTP %d", f.URL, strings.ToLower(f.Kind), f.Status)
	default:
		return f.URL
	}
}

// Observations is everything the browser saw while loading one page.
type Observations struct {
	// NavigationError is set when the browser could not navigate at all.
	NavigationError string
	// DocumentStatus and DocumentURL describe the main document's response,
	// after redirects. A zero status means no document response was seen.
	DocumentStatus int
	DocumentURL    string
	// Loaded records whether the load event fired.
	Loaded bool

	Title        string
	TextLength   int
	ElementCount int
	// VisualCount counts elements that paint without text -- a canvas, an
	// image, inline SVG, a video. A page with none of those and no text has
	// rendered nothing a user could see.
	VisualCount int

	Exceptions     []string
	ConsoleErrors  []string
	FailedRequests []FailedRequest

	Screenshot []byte
}

// Visit navigates a page and reports what the browser saw.
//
// Everything here is observed rather than asserted: deciding what counts as a
// failure is the harness's job, and a check that reads the same signals can
// weigh them differently without changing the browser code.
func Visit(ctx context.Context, client *Client, pageURL string, settle time.Duration) (*Observations, error) {
	observer := &observer{requests: make(map[string]request)}
	loaded := make(chan struct{})
	var once sync.Once

	client.On("Page.loadEventFired", func(json.RawMessage) { once.Do(func() { close(loaded) }) })
	client.On("Runtime.exceptionThrown", observer.exceptionThrown)
	client.On("Runtime.consoleAPICalled", observer.consoleAPICalled)
	client.On("Network.requestWillBeSent", observer.requestWillBeSent)
	client.On("Network.responseReceived", observer.responseReceived)
	client.On("Network.loadingFailed", observer.loadingFailed)

	for _, domain := range []string{"Page.enable", "Runtime.enable", "Network.enable"} {
		if err := client.Call(ctx, domain, nil, nil); err != nil {
			return nil, err
		}
	}

	var navigation struct {
		FrameID   string `json:"frameId"`
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := client.Call(ctx, "Page.navigate", map[string]any{"url": pageURL}, &navigation); err != nil {
		return nil, err
	}

	result := &Observations{NavigationError: navigation.ErrorText}
	if navigation.ErrorText != "" {
		// Nothing rendered, so there is nothing further to measure. This is a
		// result, not an error: the harness has to report it.
		observer.fill(result, navigation.LoaderID)
		return result, nil
	}

	select {
	case <-loaded:
		result.Loaded = true
	case <-ctx.Done():
		// A page that never finishes loading is still worth photographing:
		// the screenshot and the errors collected so far are the evidence.
		result.Loaded = false
	}

	// Settle time catches what arrives after the load event: a framework that
	// renders on mount, a fetch that fails, an exception from a timer. Without
	// it the most common agent regressions land after the measurement.
	if result.Loaded && settle > 0 {
		select {
		case <-time.After(settle):
		case <-ctx.Done():
		}
	}

	// The remaining steps run on a context that may already be done, so they
	// get their own short budget: evidence from a slow page beats none.
	measure, cancel := detachedTimeout(ctx, 20*time.Second)
	defer cancel()

	if err := measureDocument(measure, client, result); err != nil {
		result.ConsoleErrors = append(result.ConsoleErrors, "could not measure the page: "+err.Error())
	}
	if shot, err := captureScreenshot(measure, client); err == nil {
		result.Screenshot = shot
	}

	observer.fill(result, navigation.LoaderID)
	return result, nil
}

// detachedTimeout gives a step its own deadline even if ctx is already done.
func detachedTimeout(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithTimeout(ctx, budget)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

// measureDocument asks the page what it actually rendered.
const measureExpression = `(() => {
  const body = document.body;
  const text = body ? (body.innerText || "").trim() : "";
  return JSON.stringify({
    title: document.title || "",
    text: text.length,
    elements: body ? body.querySelectorAll("*").length : 0,
    visuals: body ? body.querySelectorAll("canvas, svg, img, video").length : 0
  });
})()`

func measureDocument(ctx context.Context, client *Client, result *Observations) error {
	var evaluated struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{"expression": measureExpression, "returnByValue": true}
	if err := client.Call(ctx, "Runtime.evaluate", params, &evaluated); err != nil {
		return err
	}
	if evaluated.ExceptionDetails != nil {
		return fmt.Errorf("the page rejected the measurement: %s", evaluated.ExceptionDetails.Text)
	}

	var measured struct {
		Title    string `json:"title"`
		Text     int    `json:"text"`
		Elements int    `json:"elements"`
		Visuals  int    `json:"visuals"`
	}
	if err := json.Unmarshal([]byte(evaluated.Result.Value), &measured); err != nil {
		return fmt.Errorf("unreadable measurement: %w", err)
	}
	result.Title = measured.Title
	result.TextLength, result.ElementCount, result.VisualCount = measured.Text, measured.Elements, measured.Visuals
	return nil
}

// captureScreenshot photographs the page as a user would see it.
func captureScreenshot(ctx context.Context, client *Client) ([]byte, error) {
	var shot struct {
		Data string `json:"data"`
	}
	if err := client.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, &shot); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(shot.Data)
}

// request is what is known about one in-flight request.
type request struct {
	url  string
	kind string
}

// observer accumulates DevTools events. Its methods run on the reading
// goroutine, so every field is behind the mutex.
type observer struct {
	mu         sync.Mutex
	requests   map[string]request
	exceptions []string
	console    []string
	failures   []FailedRequest
	responses  []documentResponse
}

// documentResponse is a main-document response, kept until the loader it
// belongs to is known.
type documentResponse struct {
	loaderID string
	url      string
	status   int
}

func (o *observer) exceptionThrown(params json.RawMessage) {
	var event struct {
		ExceptionDetails struct {
			Text      string `json:"text"`
			URL       string `json:"url"`
			Line      int    `json:"lineNumber"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if json.Unmarshal(params, &event) != nil {
		return
	}

	detail := event.ExceptionDetails.Text
	if event.ExceptionDetails.Exception != nil && event.ExceptionDetails.Exception.Description != "" {
		detail = event.ExceptionDetails.Exception.Description
	}
	if location := event.ExceptionDetails.URL; location != "" {
		detail = fmt.Sprintf("%s (%s:%d)", detail, location, event.ExceptionDetails.Line+1)
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.exceptions = append(o.exceptions, strings.TrimSpace(detail))
}

func (o *observer) consoleAPICalled(params json.RawMessage) {
	var event struct {
		Type string `json:"type"`
		Args []struct {
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"args"`
	}
	if json.Unmarshal(params, &event) != nil {
		return
	}
	if event.Type != "error" && event.Type != "assert" {
		// Warnings and logs are not failures. A page is allowed to be chatty.
		return
	}

	parts := make([]string, 0, len(event.Args))
	for _, arg := range event.Args {
		switch {
		case len(arg.Value) > 0:
			var text string
			if json.Unmarshal(arg.Value, &text) == nil {
				parts = append(parts, text)
			} else {
				parts = append(parts, string(arg.Value))
			}
		case arg.Description != "":
			parts = append(parts, arg.Description)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, " "))
	if text == "" {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.console = append(o.console, text)
}

func (o *observer) requestWillBeSent(params json.RawMessage) {
	var event struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if json.Unmarshal(params, &event) != nil {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests[event.RequestID] = request{url: event.Request.URL, kind: event.Type}
}

func (o *observer) responseReceived(params json.RawMessage) {
	var event struct {
		RequestID string `json:"requestId"`
		LoaderID  string `json:"loaderId"`
		Type      string `json:"type"`
		Response  struct {
			URL    string `json:"url"`
			Status int    `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(params, &event) != nil {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if event.Type == "Document" {
		o.responses = append(o.responses, documentResponse{
			loaderID: event.LoaderID,
			url:      event.Response.URL,
			status:   event.Response.Status,
		})
	}
	if event.Response.Status >= 400 && !ignorableURL(event.Response.URL) {
		o.failures = append(o.failures, FailedRequest{
			URL:    event.Response.URL,
			Kind:   event.Type,
			Status: event.Response.Status,
		})
	}
}

func (o *observer) loadingFailed(params json.RawMessage) {
	var event struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
		ErrorText string `json:"errorText"`
		Canceled  bool   `json:"canceled"`
	}
	if json.Unmarshal(params, &event) != nil {
		return
	}
	if event.Canceled {
		// A navigation away cancels in-flight requests; that is not a fault.
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	pending := o.requests[event.RequestID]
	if ignorableURL(pending.url) {
		return
	}
	kind := event.Type
	if kind == "" {
		kind = pending.kind
	}
	o.failures = append(o.failures, FailedRequest{
		URL:   pending.url,
		Kind:  kind,
		Error: event.ErrorText,
	})
}

// fill copies the collected events onto the result, resolving which document
// response belongs to the navigation.
func (o *observer) fill(result *Observations, loaderID string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	result.Exceptions = append(result.Exceptions, o.exceptions...)
	result.ConsoleErrors = append(result.ConsoleErrors, o.console...)
	result.FailedRequests = append(result.FailedRequests, o.failures...)

	for _, response := range o.responses {
		if response.loaderID == loaderID {
			result.DocumentStatus, result.DocumentURL = response.status, response.url
			return
		}
	}
	// No response carried the navigation's loader id, which happens when the
	// document came from the cache or the loader id was not reported. The
	// first document response is the best available answer.
	if len(o.responses) > 0 {
		result.DocumentStatus, result.DocumentURL = o.responses[0].status, o.responses[0].url
	}
}

// ignorableURL reports whether a request's failure says nothing about the
// page's own correctness.
func ignorableURL(url string) bool {
	switch {
	case url == "":
		return true
	case strings.HasPrefix(url, "data:"), strings.HasPrefix(url, "about:"), strings.HasPrefix(url, "chrome"):
		return true
	case strings.HasSuffix(url, "/favicon.ico"):
		// Chrome asks for a favicon whether or not the page references one, so
		// its absence is the browser's doing rather than the page's.
		return true
	default:
		return false
	}
}
