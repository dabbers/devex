package wsframe

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// pair returns a client and a server talking to each other over a pipe, each
// configured the way its role requires: the client masks, the server does not.
func pair(t *testing.T, opts Options) (client, server *Conn) {
	t.Helper()

	clientSide, serverSide := net.Pipe()
	wrap := func(conn net.Conn, mask bool) *Conn {
		scoped := opts
		scoped.Mask = mask
		buf := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		return New(conn, buf, scoped)
	}
	client, server = wrap(clientSide, true), wrap(serverSide, false)
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	return client, server
}

// send writes on one side and reads on the other, which a pipe requires be
// done concurrently.
func send(t *testing.T, from, to *Conn, opcode byte, payload []byte) (byte, []byte) {
	t.Helper()

	errs := make(chan error, 1)
	go func() { errs <- from.Write(opcode, payload) }()

	gotOp, got, err := to.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("write: %v", err)
	}
	return gotOp, got
}

func TestBothRolesReadEachOther(t *testing.T) {
	t.Parallel()
	client, server := pair(t, Options{})

	// A client must mask its frames and a server must not. Getting this
	// backwards is the mistake that makes one end hang, so both directions
	// are exercised rather than just the one the harness uses.
	opcode, got := send(t, client, server, OpText, []byte(`{"id":1,"method":"Page.enable"}`))
	if opcode != OpText || string(got) != `{"id":1,"method":"Page.enable"}` {
		t.Fatalf("server read opcode %#x %q", opcode, got)
	}

	opcode, got = send(t, server, client, OpText, []byte(`{"id":1,"result":{}}`))
	if opcode != OpText || string(got) != `{"id":1,"result":{}}` {
		t.Fatalf("client read opcode %#x %q", opcode, got)
	}

	opcode, got = send(t, client, server, OpBinary, []byte{0x00, 0xFF, 0x7F, 0x80})
	if opcode != OpBinary || !bytes.Equal(got, []byte{0x00, 0xFF, 0x7F, 0x80}) {
		t.Fatalf("binary round trip gave opcode %#x %v", opcode, got)
	}
}

func TestPayloadsAcrossEveryLengthEncoding(t *testing.T) {
	t.Parallel()

	// The three length encodings are the part of the framing most likely to be
	// wrong, and a screenshot lands squarely in the largest of them.
	for _, size := range []int{0, 1, 125, 126, 127, 65535, 65536, 300000} {
		client, server := pair(t, Options{MaxMessage: 1 << 20})
		payload := bytes.Repeat([]byte("ab"), size/2+size%2)[:size]

		_, got := send(t, client, server, OpBinary, payload)
		if !bytes.Equal(got, payload) {
			t.Fatalf("a %d-byte payload came back as %d bytes", size, len(got))
		}
	}
}

func TestAMaskedWriteLeavesTheCallersPayloadAlone(t *testing.T) {
	t.Parallel()
	client, server := pair(t, Options{})

	payload := []byte("the caller keeps reading this")
	original := string(payload)
	send(t, client, server, OpText, payload)
	if string(payload) != original {
		t.Fatalf("masking mutated the caller's slice: %q", payload)
	}
}

func TestAPingIsAnsweredAndTheMessageStillArrives(t *testing.T) {
	t.Parallel()
	client, server := pair(t, Options{})

	go func() {
		_ = server.Write(OpPing, []byte("keepalive"))
		_ = server.Write(OpText, []byte("after the ping"))
	}()

	// The client answers the ping from inside ReadMessage, so the pong has to
	// be drained from the server side before the text message is read.
	done := make(chan struct{})
	go func() {
		defer close(done)
		opcode, got, err := client.ReadMessage()
		if err != nil {
			t.Errorf("client read: %v", err)
			return
		}
		if opcode != OpText || string(got) != "after the ping" {
			t.Errorf("client read opcode %#x %q", opcode, got)
		}
	}()

	if err := server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	pong, err := server.readFrame()
	if err != nil {
		t.Fatalf("reading the pong: %v", err)
	}
	if pong.opcode != OpPong {
		t.Fatalf("the ping was answered with opcode %#x", pong.opcode)
	}
	// A pong must echo the ping's payload, which is how a peer tells its own
	// keepalive apart from someone else's.
	if string(pong.payload) != "keepalive" {
		t.Fatalf("the pong carried %q", pong.payload)
	}
	<-done
}

func TestAMessageBeyondTheLimitIsRefused(t *testing.T) {
	t.Parallel()
	client, server := pair(t, Options{MaxMessage: 1024})

	go func() { _ = client.Write(OpBinary, bytes.Repeat([]byte("x"), 4096)) }()
	if _, _, err := server.ReadMessage(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("an oversized frame gave err = %v", err)
	}
}

func TestAClosedPeerIsReportedAsClosed(t *testing.T) {
	t.Parallel()
	client, server := pair(t, Options{})

	go func() { _ = client.Close() }()
	if _, _, err := server.ReadMessage(); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close gave err = %v, want ErrClosed", err)
	}
	if err := client.Write(OpText, []byte("gone")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close gave err = %v, want ErrClosed", err)
	}
	// Closing twice is what a deferred Close plus an explicit one does.
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestAFragmentedMessageIsReassembled(t *testing.T) {
	t.Parallel()
	client, server := pair(t, Options{})

	go func() {
		// A continuation sequence written by hand: FIN is clear on all but the
		// last frame, and only the first carries the opcode.
		_ = writeRawFrame(client, false, OpText, []byte("one "))
		_ = writeRawFrame(client, false, OpContinuation, []byte("two "))
		_ = writeRawFrame(client, true, OpContinuation, []byte("three"))
	}()

	opcode, got, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if opcode != OpText || string(got) != "one two three" {
		t.Fatalf("reassembled opcode %#x %q", opcode, got)
	}
}

// writeRawFrame writes one frame with FIN under the test's control, which the
// public Write deliberately does not allow.
func writeRawFrame(c *Conn, fin bool, opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	first := opcode
	if fin {
		first |= 0x80
	}
	header := []byte{first, 0x80 | byte(len(payload)), 0, 0, 0, 0}
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
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
