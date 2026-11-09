// Package browser drives a real Chromium over the Chrome DevTools Protocol.
//
// The verification harness needs a browser that renders the way a user's
// browser renders, and it needs to see what went wrong when a page is broken:
// uncaught exceptions, console errors, subresources that failed to load, a
// document that loaded but painted nothing. Only the browser knows those, so
// the harness talks to one rather than fetching HTML and guessing.
//
// There is no driver dependency. DevTools is a JSON protocol over a websocket,
// the framing is already in this tree, and the handful of commands a
// verification needs is smaller than the surface a driver library would add.
package browser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/dabbers/devex/internal/wsframe"
)

// maxMessageBytes bounds one DevTools message. Screenshots are by far the
// largest thing the protocol sends back, so this is generous.
const maxMessageBytes = 64 << 20

// ErrClosed reports that the DevTools connection is gone.
var ErrClosed = errors.New("browser: devtools connection closed")

// Client is a DevTools connection to one target.
type Client struct {
	framed *wsframe.Conn

	mu       sync.Mutex
	nextID   int
	pending  map[int]chan response
	handlers map[string][]func(json.RawMessage)
	closed   bool
	readErr  error
}

// response is one DevTools reply.
type response struct {
	Result json.RawMessage
	Err    error
}

// message is a DevTools frame in either direction.
type message struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Dial connects to a DevTools websocket endpoint and starts reading it.
func Dial(wsURL string, timeout time.Duration) (*Client, error) {
	framed, err := dialWebSocket(wsURL, timeout)
	if err != nil {
		return nil, err
	}
	client := &Client{
		framed:   framed,
		pending:  make(map[int]chan response),
		handlers: make(map[string][]func(json.RawMessage)),
	}
	go client.read()
	return client, nil
}

// dialWebSocket performs the client half of the RFC 6455 handshake.
func dialWebSocket(rawURL string, timeout time.Duration) (*wsframe.Conn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("browser: unreadable devtools URL %q: %w", rawURL, err)
	}
	if parsed.Scheme != "ws" {
		// DevTools is reached over the loopback interface of the machine the
		// browser runs on, so there is no TLS to speak and no reason to
		// pretend this handles it.
		return nil, fmt.Errorf("browser: %q is not a ws:// URL", rawURL)
	}

	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(address, "80")
	}
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, fmt.Errorf("browser: dial devtools: %w", err)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("browser: handshake nonce: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	request := "GET " + parsed.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("browser: write handshake: %w", err)
	}

	reader := bufio.NewReaderSize(conn, 32<<10)
	// The body of a 101 is the websocket stream itself: closing it would close
	// the connection this function exists to hand back.
	resp, err := http.ReadResponse(reader, nil) //nolint:bodyclose // the body is the socket
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("browser: read handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("browser: devtools refused the upgrade with %s", resp.Status)
	}
	sum := sha1.Sum([]byte(key + wsframe.Magic)) //nolint:gosec // the handshake fixes SHA-1
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		// A wrong accept value means whatever answered is not speaking this
		// protocol, so nothing read from it can be trusted.
		_ = conn.Close()
		return nil, errors.New("browser: devtools handshake did not verify")
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}

	buf := bufio.NewReadWriter(reader, bufio.NewWriter(conn))
	return wsframe.New(conn, buf, wsframe.Options{Mask: true, MaxMessage: maxMessageBytes}), nil
}

// On registers a handler for an event method.
//
// Handlers run on the reading goroutine, so a handler must not call Call: the
// reply it waited for could only arrive through the reader it is blocking.
// Recording a value or closing a channel is what these are for.
func (c *Client) On(method string, fn func(params json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[method] = append(c.handlers[method], fn)
}

// Call sends a command and waits for its reply.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	payload := message{Method: method}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("browser: encode %s params: %w", method, err)
		}
		payload.Params = encoded
	}

	reply := make(chan response, 1)
	c.mu.Lock()
	if c.closed {
		err := c.readErr
		c.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return err
	}
	c.nextID++
	payload.ID = c.nextID
	c.pending[payload.ID] = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, payload.ID)
		c.mu.Unlock()
	}()

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("browser: encode %s: %w", method, err)
	}
	if err := c.framed.Write(wsframe.OpText, encoded); err != nil {
		return fmt.Errorf("browser: send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("browser: %s: %w", method, ctx.Err())
	case got := <-reply:
		if got.Err != nil {
			return fmt.Errorf("browser: %s: %w", method, got.Err)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(got.Result, result); err != nil {
			return fmt.Errorf("browser: decode %s result: %w", method, err)
		}
		return nil
	}
}

// read pumps the connection, routing replies to their callers and events to
// their handlers, until the connection fails or is closed.
func (c *Client) read() {
	for {
		opcode, payload, err := c.framed.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		if opcode != wsframe.OpText {
			continue
		}

		var decoded message
		if err := json.Unmarshal(payload, &decoded); err != nil {
			// A frame that is not a protocol message is not something this
			// can recover the stream from.
			c.fail(fmt.Errorf("browser: unreadable devtools message: %w", err))
			return
		}

		if decoded.ID != 0 {
			c.deliver(decoded)
			continue
		}
		if decoded.Method == "" {
			continue
		}
		c.mu.Lock()
		handlers := c.handlers[decoded.Method]
		c.mu.Unlock()
		for _, fn := range handlers {
			fn(decoded.Params)
		}
	}
}

// deliver routes one reply to the caller waiting for it.
func (c *Client) deliver(decoded message) {
	c.mu.Lock()
	reply, ok := c.pending[decoded.ID]
	c.mu.Unlock()
	if !ok {
		// The caller gave up, which a cancelled context does; the reply has
		// nowhere to go.
		return
	}

	got := response{Result: decoded.Result}
	if decoded.Error != nil {
		got.Err = fmt.Errorf("devtools error %d: %s", decoded.Error.Code, decoded.Error.Message)
	}
	select {
	case reply <- got:
	default:
	}
}

// fail marks the connection dead and wakes everyone waiting on it.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.readErr = err
	pending := c.pending
	c.pending = make(map[int]chan response)
	c.mu.Unlock()

	for _, reply := range pending {
		select {
		case reply <- response{Err: err}:
		default:
		}
	}
}

// Close drops the DevTools connection.
func (c *Client) Close() error {
	err := c.framed.Close()
	c.fail(ErrClosed)
	return err
}
