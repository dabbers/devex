package browser

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/wsframe"
)

// fakeDevTools stands in for a browser's DevTools endpoint. It speaks the same
// protocol a real browser speaks, so the client can be tested on a machine
// with no browser on it at all -- which is most of CI.
type fakeDevTools struct {
	server *httptest.Server

	mu       sync.Mutex
	received []string
	framed   *wsframe.Conn

	// reply decides what to answer a command with. Returning a nil reply
	// answers nothing, which is how a hung command is simulated.
	reply func(id int, method string, params json.RawMessage) []byte
}

func newFakeDevTools(t *testing.T) *fakeDevTools {
	t.Helper()

	fake := &fakeDevTools{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsframe.Magic)) //nolint:gosec // the handshake fixes SHA-1
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = buf.Flush()

		framed := wsframe.New(conn, buf, wsframe.Options{MaxMessage: 1 << 20})
		fake.mu.Lock()
		fake.framed = framed
		fake.mu.Unlock()
		fake.serve(framed)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeDevTools) serve(framed *wsframe.Conn) {
	for {
		_, payload, err := framed.ReadMessage()
		if err != nil {
			return
		}
		var command struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(payload, &command) != nil {
			return
		}

		f.mu.Lock()
		f.received = append(f.received, command.Method)
		responder := f.reply
		f.mu.Unlock()

		if responder == nil {
			responder = func(id int, _ string, _ json.RawMessage) []byte {
				return fmt.Appendf(nil, `{"id":%d,"result":{}}`, id)
			}
		}
		if answer := responder(command.ID, command.Method, command.Params); answer != nil {
			if framed.Write(wsframe.OpText, answer) != nil {
				return
			}
		}
	}
}

// url is the ws:// address the client dials.
func (f *fakeDevTools) url() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/devtools/page/1"
}

// drop closes the connection from the browser's end, the way a browser that
// has crashed or been killed would. httptest does not do this for us: it stops
// tracking a connection once it has been hijacked.
func (f *fakeDevTools) drop() {
	f.mu.Lock()
	framed := f.framed
	f.mu.Unlock()
	if framed != nil {
		_ = framed.Close()
	}
}

func (f *fakeDevTools) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func dialFake(t *testing.T, fake *fakeDevTools) *Client {
	t.Helper()

	client, err := Dial(fake.url(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestACommandCarriesItsParamsAndReturnsItsResult(t *testing.T) {
	t.Parallel()

	fake := newFakeDevTools(t)
	fake.reply = func(id int, method string, params json.RawMessage) []byte {
		if method != "Page.navigate" {
			return fmt.Appendf(nil, `{"id":%d,"result":{}}`, id)
		}
		var sent struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(params, &sent)
		return fmt.Appendf(nil, `{"id":%d,"result":{"frameId":"F1","loaderId":"L1","echo":%q}}`, id, sent.URL)
	}
	client := dialFake(t, fake)

	var navigation struct {
		FrameID  string `json:"frameId"`
		LoaderID string `json:"loaderId"`
		Echo     string `json:"echo"`
	}
	err := client.Call(t.Context(), "Page.navigate",
		map[string]any{"url": "https://preview.example"}, &navigation)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if navigation.FrameID != "F1" || navigation.LoaderID != "L1" {
		t.Fatalf("result = %+v", navigation)
	}
	if navigation.Echo != "https://preview.example" {
		t.Fatalf("the params did not reach the browser: %q", navigation.Echo)
	}
}

func TestAProtocolErrorBecomesAnError(t *testing.T) {
	t.Parallel()

	fake := newFakeDevTools(t)
	fake.reply = func(id int, _ string, _ json.RawMessage) []byte {
		return fmt.Appendf(nil, `{"id":%d,"error":{"code":-32000,"message":"Cannot find context"}}`, id)
	}
	client := dialFake(t, fake)

	err := client.Call(t.Context(), "Runtime.evaluate", map[string]any{"expression": "1"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Cannot find context") {
		t.Fatalf("err = %v, want the browser's message", err)
	}
}

func TestRepliesAreMatchedToTheirOwnCallers(t *testing.T) {
	t.Parallel()

	// A verification issues commands from one goroutine and reads events on
	// another, and the browser is free to answer out of order. Handing a
	// caller someone else's reply would be silent and wrong, rather than loud
	// and wrong, so it is worth pinning down.
	fake := newFakeDevTools(t)
	fake.reply = func(id int, _ string, params json.RawMessage) []byte {
		var sent struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(params, &sent)
		// Answer later commands sooner.
		time.Sleep(time.Duration(20-sent.N) * time.Millisecond)
		return fmt.Appendf(nil, `{"id":%d,"result":{"n":%d}}`, id, sent.N)
	}
	client := dialFake(t, fake)

	var wg sync.WaitGroup
	for n := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got struct {
				N int `json:"n"`
			}
			if err := client.Call(t.Context(), "Test.echo", map[string]any{"n": n}, &got); err != nil {
				t.Errorf("call %d: %v", n, err)
				return
			}
			if got.N != n {
				t.Errorf("call %d got reply for %d", n, got.N)
			}
		}()
	}
	wg.Wait()
}

func TestEventsReachTheirHandlers(t *testing.T) {
	t.Parallel()

	fake := newFakeDevTools(t)
	fake.reply = func(id int, method string, _ json.RawMessage) []byte {
		if method == "Page.enable" {
			// Events arrive unsolicited, with no id on them.
			return []byte(`{"method":"Page.loadEventFired","params":{"timestamp":1}}`)
		}
		return fmt.Appendf(nil, `{"id":%d,"result":{}}`, id)
	}
	client := dialFake(t, fake)

	fired := make(chan json.RawMessage, 1)
	client.On("Page.loadEventFired", func(params json.RawMessage) { fired <- params })

	// The enable command is never answered, so it is expected to time out:
	// what matters is that the event it provoked was delivered.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_ = client.Call(ctx, "Page.enable", nil, nil)

	select {
	case params := <-fired:
		if !strings.Contains(string(params), "timestamp") {
			t.Fatalf("event params = %s", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the event never reached its handler")
	}
}

func TestACommandThatIsNeverAnsweredGivesUpWithTheContext(t *testing.T) {
	t.Parallel()

	fake := newFakeDevTools(t)
	fake.reply = func(int, string, json.RawMessage) []byte { return nil }
	client := dialFake(t, fake)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := client.Call(ctx, "Page.captureScreenshot", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("the command outlived its context")
	}
	if methods := fake.methods(); len(methods) != 1 || methods[0] != "Page.captureScreenshot" {
		t.Fatalf("the browser saw %v", methods)
	}
}

func TestALostConnectionWakesEveryoneWaiting(t *testing.T) {
	t.Parallel()

	// A browser that dies mid-verification must not leave the harness blocked
	// until the whole run times out: the control plane is waiting on a report.
	fake := newFakeDevTools(t)
	fake.reply = func(int, string, json.RawMessage) []byte { return nil }
	client := dialFake(t, fake)

	waiting := make(chan error, 1)
	go func() { waiting <- client.Call(context.Background(), "Page.navigate", nil, nil) }()

	time.Sleep(100 * time.Millisecond)
	fake.drop()

	select {
	case err := <-waiting:
		if err == nil {
			t.Fatal("a command on a dead connection reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command on a dead connection never returned")
	}

	// Later commands fail immediately rather than hanging on a dead socket.
	if err := client.Call(context.Background(), "Page.navigate", nil, nil); err == nil {
		t.Fatal("a command after the connection died reported success")
	}
}

func TestDialRefusesWhatIsNotADevToolsEndpoint(t *testing.T) {
	t.Parallel()

	// An endpoint that answers but does not complete the handshake is not
	// speaking this protocol, and nothing read from it would mean anything.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "not a websocket")
	}))
	defer plain.Close()

	if _, err := Dial("ws"+strings.TrimPrefix(plain.URL, "http"), 2*time.Second); err == nil {
		t.Fatal("dialling a plain HTTP server succeeded")
	}
	if _, err := Dial("https://example.invalid", time.Second); err == nil {
		t.Fatal("dialling a non-ws URL succeeded")
	}
}

func TestAHandshakeThatDoesNotVerifyIsRefused(t *testing.T) {
	t.Parallel()

	// A 101 with the wrong accept value means whatever answered did not derive
	// it from this client's key, so the stream is not this client's stream.
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: not-the-right-value\r\n\r\n")
		_ = buf.Flush()
	}))
	defer liar.Close()

	_, err := Dial("ws"+strings.TrimPrefix(liar.URL, "http"), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not verify") {
		t.Fatalf("err = %v, want a refused handshake", err)
	}
}

func TestFindResolvesABrowserOrSaysWhatItTried(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "definitely-not-a-browser")
	if _, err := Find(missing); err == nil {
		t.Fatal("Find accepted a binary that does not exist")
	}

	// An explicit binary wins over the candidate list, which is how a UI VM
	// points the harness at a browser that is not where one is expected.
	fake := filepath.Join(t.TempDir(), "my-browser")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Find(fake)
	if err != nil || got != fake {
		t.Fatalf("Find(%q) = %q, %v", fake, got, err)
	}
}

func TestTheBinaryEnvNamesTheBrowser(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "env-browser")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, fake)

	got, err := Find("")
	if err != nil || got != fake {
		t.Fatalf("Find(\"\") = %q, %v", got, err)
	}
}

func TestIgnorableURLsAreTheBrowsersDoingNotThePages(t *testing.T) {
	t.Parallel()

	for _, ignored := range []string{
		"", "data:image/png;base64,AAA", "about:blank",
		"chrome-extension://abc/x.js", "https://preview.example/favicon.ico",
	} {
		if !ignorableURL(ignored) {
			t.Errorf("ignorableURL(%q) = false", ignored)
		}
	}
	for _, reported := range []string{
		"https://preview.example/app.js",
		"https://preview.example/api/items",
		"https://preview.example/img/favicon.ico.png",
	} {
		if ignorableURL(reported) {
			t.Errorf("ignorableURL(%q) = true; a real failure would be hidden", reported)
		}
	}
}
