package api

import (
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/dabbers/devex/internal/wsframe"
)

// The server half of the terminal websocket: the handshake, which is specific
// to answering an upgrade request, plus the Origin check that makes it safe to
// answer at all. The framing underneath is shared with the verification
// harness's DevTools client, which speaks the same protocol from the other end.

// maxMessageBytes bounds a single inbound message. A terminal's input is
// keystrokes; anything approaching this is not a person typing.
const maxMessageBytes = 1 << 20

// ErrWSClosed reports that the peer closed the connection.
var ErrWSClosed = wsframe.ErrClosed

// wsConn is one upgraded connection.
type wsConn struct {
	framed *wsframe.Conn
}

// upgrade performs the handshake and takes over the connection.
//
// The Origin check is the security boundary. A browser will send a socket
// request with the page's origin attached and cannot suppress it, so this is
// what stops any page the operator visits from opening a shell on their
// machine.
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!headerContainsToken(r.Header.Get("Connection"), "upgrade") {
		return nil, errors.New("api: not a websocket upgrade request")
	}
	if r.Header.Get("Sec-Websocket-Version") != "13" {
		return nil, errors.New("api: unsupported websocket version")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return nil, errors.New("api: missing websocket key")
	}
	if err := checkOrigin(r); err != nil {
		return nil, err
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("api: connection cannot be hijacked")
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("api: hijack: %w", err)
	}

	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	// A handshake that cannot be written leaves nothing to recover; the error
	// being returned is the one that matters.
	if _, err := buf.WriteString(response); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("api: write handshake: %w", err)
	}
	if err := buf.Flush(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("api: flush handshake: %w", err)
	}
	return &wsConn{framed: wsframe.New(conn, buf, wsframe.Options{MaxMessage: maxMessageBytes})}, nil
}

// acceptKey derives the handshake response value from the client's key.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsframe.Magic)) //nolint:gosec // the handshake fixes SHA-1
	return base64.StdEncoding.EncodeToString(sum[:])
}

// checkOrigin rejects a socket opened from another site.
func checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients send no Origin. They are not the threat this
		// guards against: a page cannot suppress the header.
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("api: unreadable Origin %q", origin)
	}
	if !strings.EqualFold(parsed.Host, r.Host) {
		return fmt.Errorf("api: refusing a websocket from origin %q", origin)
	}
	return nil
}

// headerContainsToken reports whether a comma-separated header lists a token.
func headerContainsToken(header, token string) bool {
	for part := range strings.SplitSeq(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// readMessage returns the next complete application message.
func (c *wsConn) readMessage() (opcode byte, payload []byte, err error) {
	return c.framed.ReadMessage()
}

// writeBinary sends an application message of raw bytes. Terminal output is
// not necessarily valid UTF-8, so it travels as binary rather than text.
func (c *wsConn) writeBinary(payload []byte) error { return c.framed.WriteBinary(payload) }

// writeText sends a UTF-8 application message.
func (c *wsConn) writeText(payload string) error { return c.framed.WriteText(payload) }

// Close sends a close frame and drops the connection.
func (c *wsConn) Close() error { return c.framed.Close() }

// opText names the framing layer's text message kind for this package.
const opText = wsframe.OpText
