// Package wsframe implements the RFC 6455 framing layer.
//
// It is hand-written rather than taken as a dependency because the whole
// surface needed is one connection carrying a couple of message kinds, and it
// is shared because two callers need the same framing from opposite ends: the
// control plane's terminal socket is a server, and the verification harness's
// DevTools client is a client. The only difference the protocol draws between
// them is masking -- a client must mask its frames and a server must not -- so
// that is a flag rather than a second implementation.
//
// The handshake is not here. A server answers an upgrade request and a client
// sends one; each side does its own and hands the taken-over connection over.
package wsframe

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Opcodes.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// Magic is the GUID RFC 6455 fixes for the handshake. Both roles compute the
// same accept value from it, so it lives with the framing.
const Magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// DefaultMaxMessage bounds a reassembled message when a caller names no limit.
const DefaultMaxMessage = 1 << 20

// defaultWriteTimeout keeps a stuck peer from blocking a writer forever.
const defaultWriteTimeout = 10 * time.Second

// ErrClosed reports that the peer closed the connection.
var ErrClosed = errors.New("wsframe: websocket closed")

// Options configures a connection.
type Options struct {
	// Mask says whether outbound frames are masked. A client must set it; a
	// server must not.
	Mask bool
	// MaxMessage bounds one reassembled message. Zero means
	// DefaultMaxMessage. The limit exists so a malformed length header cannot
	// make the process allocate without bound.
	MaxMessage int
	// WriteTimeout bounds one frame write. Zero means defaultWriteTimeout.
	WriteTimeout time.Duration
}

// Conn is one framed connection.
type Conn struct {
	conn net.Conn
	buf  *bufio.ReadWriter
	opts Options

	// writeMu serialises writes: application frames and pongs are produced by
	// different goroutines, and interleaving two frames corrupts the stream.
	writeMu sync.Mutex
	closed  bool

	// messageOp remembers the opcode of a fragmented message in progress so a
	// continuation frame knows what it is continuing.
	messageOp byte
}

// New wraps a connection that has already been upgraded.
func New(conn net.Conn, buf *bufio.ReadWriter, opts Options) *Conn {
	if opts.MaxMessage <= 0 {
		opts.MaxMessage = DefaultMaxMessage
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	return &Conn{conn: conn, buf: buf, opts: opts}
}

// SetReadDeadline bounds how long the next read may block.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// ReadMessage returns the next complete application message, answering control
// frames as it goes. Fragmented messages are reassembled.
func (c *Conn) ReadMessage() (opcode byte, payload []byte, err error) {
	var assembled []byte
	for {
		frame, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch frame.opcode {
		case OpClose:
			return 0, nil, ErrClosed
		case OpPing:
			// A pong must echo the ping's payload.
			if err := c.Write(OpPong, frame.payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpText, OpBinary:
			c.messageOp = frame.opcode
			assembled = frame.payload
		case OpContinuation:
			if c.messageOp == 0 {
				return 0, nil, errors.New("wsframe: continuation frame with nothing to continue")
			}
			assembled = append(assembled, frame.payload...)
		default:
			return 0, nil, fmt.Errorf("wsframe: unsupported opcode %#x", frame.opcode)
		}

		if len(assembled) > c.opts.MaxMessage {
			return 0, nil, errors.New("wsframe: message too large")
		}
		if frame.fin {
			op := c.messageOp
			c.messageOp = 0
			return op, assembled, nil
		}
	}
}

type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// readFrame reads and unmasks one frame.
func (c *Conn) readFrame() (frame, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.buf, header[:]); err != nil {
		return frame{}, err
	}

	read := frame{
		fin:    header[0]&0x80 != 0,
		opcode: header[0] & 0x0F,
	}
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7F)

	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.buf, extended[:]); err != nil {
			return frame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.buf, extended[:]); err != nil {
			return frame{}, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > uint64(c.opts.MaxMessage) {
		return frame{}, errors.New("wsframe: frame too large")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.buf, mask[:]); err != nil {
			return frame{}, err
		}
	}

	read.payload = make([]byte, length)
	if _, err := io.ReadFull(c.buf, read.payload); err != nil {
		return frame{}, err
	}
	if masked {
		for i := range read.payload {
			read.payload[i] ^= mask[i%4]
		}
	}
	return read, nil
}

// Write sends one unfragmented frame.
func (c *Conn) Write(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return ErrClosed
	}

	header := []byte{0x80 | opcode}
	flag := byte(0)
	if c.opts.Mask {
		flag = 0x80
	}
	switch size := len(payload); {
	case size < 126:
		header = append(header, flag|byte(size))
	case size <= 0xFFFF:
		header = append(header, flag|126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(size))
	default:
		header = append(header, flag|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(size))
	}

	body := payload
	if c.opts.Mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return fmt.Errorf("wsframe: mask key: %w", err)
		}
		header = append(header, key[:]...)
		// Masking is applied to a copy: the payload belongs to the caller.
		body = make([]byte, len(payload))
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout)); err != nil {
		return err
	}
	if _, err := c.buf.Write(header); err != nil {
		return err
	}
	if _, err := c.buf.Write(body); err != nil {
		return err
	}
	return c.buf.Flush()
}

// WriteBinary sends an application message of raw bytes.
func (c *Conn) WriteBinary(payload []byte) error { return c.Write(OpBinary, payload) }

// WriteText sends a UTF-8 application message.
func (c *Conn) WriteText(payload string) error { return c.Write(OpText, []byte(payload)) }

// Close sends a close frame and drops the connection.
func (c *Conn) Close() error {
	c.writeMu.Lock()
	if c.closed {
		c.writeMu.Unlock()
		return nil
	}
	c.closed = true
	c.writeMu.Unlock()

	// Best effort: the peer may already be gone. A close frame from a client
	// has to be masked like any other, but it carries no payload, so the mask
	// is applied to nothing and a zero key is as good as a random one.
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	if c.opts.Mask {
		_, _ = c.buf.Write([]byte{0x80 | OpClose, 0x80, 0, 0, 0, 0})
	} else {
		_, _ = c.buf.Write([]byte{0x80 | OpClose, 0})
	}
	_ = c.buf.Flush()
	return c.conn.Close()
}
