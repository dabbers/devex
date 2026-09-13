package api

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 server, enough to carry a terminal.
//
// This is hand-written rather than taken as a dependency because the whole
// surface needed here is one endpoint carrying two message kinds. It handles
// what a browser actually sends: masked client frames, fragmentation, and the
// control frames a connection needs to stay alive and close cleanly.

// WebSocket opcodes.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// wsMagic is the GUID RFC 6455 fixes for the handshake.
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxMessageBytes bounds a single inbound message. A terminal's input is
// keystrokes; anything approaching this is not a person typing.
const maxMessageBytes = 1 << 20

// ErrWSClosed reports that the peer closed the connection.
var ErrWSClosed = errors.New("api: websocket closed")

// wsConn is one upgraded connection.
type wsConn struct {
	conn net.Conn
	buf  *bufio.ReadWriter

	// writeMu serialises writes: output frames and pongs are produced by
	// different goroutines, and interleaving two frames corrupts the stream.
	writeMu sync.Mutex
	closed  bool
}

// upgrade performs the handshake and takes over the connection.
//
// The Origin check is the security boundary. The control plane has no
// authentication of its own, so without it any page the operator visits could
// open a socket to a shell on their machine.
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

	sum := sha1.Sum([]byte(key + wsMagic)) //nolint:gosec // the handshake fixes SHA-1
	accept := base64.StdEncoding.EncodeToString(sum[:])

	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
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
	return &wsConn{conn: conn, buf: buf}, nil
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
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// readMessage returns the next complete application message, answering control
// frames as it goes. Fragmented messages are reassembled.
func (c *wsConn) readMessage() (opcode byte, payload []byte, err error) {
	var (
		assembled []byte
		messageOp byte
	)
	for {
		frame, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch frame.opcode {
		case opClose:
			return 0, nil, ErrWSClosed
		case opPing:
			// A pong must echo the ping's payload.
			if err := c.write(opPong, frame.payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opText, opBinary:
			messageOp = frame.opcode
			assembled = frame.payload
		case opContinuation:
			if messageOp == 0 {
				return 0, nil, errors.New("api: continuation frame with nothing to continue")
			}
			assembled = append(assembled, frame.payload...)
		default:
			return 0, nil, fmt.Errorf("api: unsupported websocket opcode %#x", frame.opcode)
		}

		if len(assembled) > maxMessageBytes {
			return 0, nil, errors.New("api: websocket message too large")
		}
		if frame.fin {
			return messageOp, assembled, nil
		}
	}
}

type wsFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// readFrame reads and unmasks one frame.
func (c *wsConn) readFrame() (wsFrame, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.buf, header[:]); err != nil {
		return wsFrame{}, err
	}

	frame := wsFrame{
		fin:    header[0]&0x80 != 0,
		opcode: header[0] & 0x0F,
	}
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7F)

	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.buf, extended[:]); err != nil {
			return wsFrame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.buf, extended[:]); err != nil {
			return wsFrame{}, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > maxMessageBytes {
		return wsFrame{}, errors.New("api: websocket frame too large")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.buf, mask[:]); err != nil {
			return wsFrame{}, err
		}
	}

	frame.payload = make([]byte, length)
	if _, err := io.ReadFull(c.buf, frame.payload); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i := range frame.payload {
			frame.payload[i] ^= mask[i%4]
		}
	}
	return frame, nil
}

// write sends one unfragmented frame. Server frames are never masked.
func (c *wsConn) write(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return ErrWSClosed
	}

	header := []byte{0x80 | opcode}
	switch size := len(payload); {
	case size < 126:
		header = append(header, byte(size))
	case size <= 0xFFFF:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(size))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(size))
	}

	// A stuck reader must not block the writer forever.
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := c.buf.Write(header); err != nil {
		return err
	}
	if _, err := c.buf.Write(payload); err != nil {
		return err
	}
	return c.buf.Flush()
}

// writeBinary sends an application message of raw bytes. Terminal output is
// not necessarily valid UTF-8, so it travels as binary rather than text.
func (c *wsConn) writeBinary(payload []byte) error { return c.write(opBinary, payload) }

// writeText sends a UTF-8 application message.
func (c *wsConn) writeText(payload string) error { return c.write(opText, []byte(payload)) }

// Close sends a close frame and drops the connection.
func (c *wsConn) Close() error {
	c.writeMu.Lock()
	if c.closed {
		c.writeMu.Unlock()
		return nil
	}
	c.closed = true
	c.writeMu.Unlock()

	// Best effort: the peer may already be gone.
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.buf.Write([]byte{0x80 | opClose, 0})
	_ = c.buf.Flush()
	return c.conn.Close()
}
