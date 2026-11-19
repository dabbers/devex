// Package guestproto is the wire protocol between the Firecracker driver on the
// host and the agent inside each guest (cmd/dabberz-guest).
//
// The transport is virtio-vsock. Firecracker exposes a guest's vsock as a Unix
// socket on the host; the host writes "CONNECT <port>\n", Firecracker answers
// "OK <n>\n", and from then on the stream is an ordinary connection to the
// guest agent. vsock rather than SSH because it needs no keys to provision,
// works with the guest's network down or misconfigured, and cannot be reached
// from the guest network -- neither by another guest nor by anything the
// customer's code exposes.
//
// On that connection each side writes newline-delimited JSON. The host sends
// one Request, then (for sessions) Frames; the agent answers with Frames,
// ending with exactly one of kind Exit. One connection carries one command, so
// there is no multiplexing to get wrong.
package guestproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Port is the vsock port the agent listens on.
const Port = 1024

// Request kinds.
const (
	// KindPing asks the agent to answer with an immediate zero exit. The
	// driver uses it to learn that a freshly booted guest can run commands.
	KindPing = "ping"
	// KindExec runs a command with piped stdio and waits for it.
	KindExec = "exec"
	// KindSession runs a command on a pseudo-terminal.
	KindSession = "session"
)

// Request opens a connection.
type Request struct {
	Kind string            `json:"kind"`
	Argv []string          `json:"argv,omitempty"`
	Dir  string            `json:"dir,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// Stdin is fed to an exec and then closed.
	Stdin []byte `json:"stdin,omitempty"`
	// TimeoutMillis bounds an exec; the agent kills the process group at the
	// deadline and reports TimedOut. Zero means no deadline.
	TimeoutMillis int64  `json:"timeout_ms,omitempty"`
	Cols          uint16 `json:"cols,omitempty"`
	Rows          uint16 `json:"rows,omitempty"`
}

// Frame kinds.
const (
	FrameStdout = "out"
	FrameStderr = "err"
	FrameExit   = "exit"
	// FrameStdin and FrameResize flow host to guest during a session.
	FrameStdin  = "in"
	FrameResize = "resize"
)

// Frame is one message after the request.
type Frame struct {
	Kind string `json:"t"`
	Data []byte `json:"d,omitempty"`
	// Code, TimedOut and Error are set on an Exit frame. Error reports that
	// the command could not be run at all, which is different from it running
	// and failing.
	Code     int    `json:"code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
	Cols     uint16 `json:"cols,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
}

// MaxLine bounds one encoded message. Output is chunked well below it, so
// only a hostile or broken peer reaches it.
const MaxLine = 8 << 20

// Encoder writes messages; it is safe for concurrent use because stdout and
// stderr are forwarded from separate goroutines.
type Encoder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{enc: json.NewEncoder(w)} }

// Encode writes one message followed by a newline.
func (e *Encoder) Encode(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enc.Encode(v)
}

// Decoder reads messages.
type Decoder struct{ scanner *bufio.Scanner }

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), MaxLine)
	return &Decoder{scanner: scanner}
}

// Decode reads the next message into v. It returns io.EOF at a clean end.
func (d *Decoder) Decode(v any) error {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	if err := json.Unmarshal(d.scanner.Bytes(), v); err != nil {
		return fmt.Errorf("guestproto: decode: %w", err)
	}
	return nil
}

// ChunkWriter turns a byte stream into frames of one kind, so a process's
// output can be pointed straight at the connection.
type ChunkWriter struct {
	Enc  *Encoder
	Kind string
}

// chunk keeps frames small enough that a slow reader never forces a large
// allocation on either side.
const chunk = 32 << 10

// Write implements io.Writer.
func (w ChunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), chunk)
		if err := w.Enc.Encode(Frame{Kind: w.Kind, Data: p[:n]}); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
