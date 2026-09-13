package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// wsClient is a minimal client for driving the server's socket in tests.
type wsClient struct {
	conn net.Conn
}

// dialShell performs a handshake against the test server.
func dialShell(t *testing.T, f *fixture, path string, headers map[string]string) (*wsClient, *http.Response) {
	t.Helper()

	address := strings.TrimPrefix(f.server.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	key := make([]byte, 16)
	//nolint:errcheck // test entropy
	rand.Read(key)
	encoded := base64.StdEncoding.EncodeToString(key)

	request := "GET " + path + " HTTP/1.1\r\nHost: " + address + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + encoded + "\r\n"
	for name, value := range headers {
		request += name + ": " + value + "\r\n"
	}
	request += "\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	reader := newBufReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return &wsClient{conn: conn}, resp
}

// send writes a masked client text frame, as a browser would.
func (c *wsClient) send(t *testing.T, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	frame := []byte{0x81}
	size := len(body)
	switch {
	case size < 126:
		frame = append(frame, byte(0x80|size))
	default:
		frame = append(frame, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(frame[2:], uint16(size))
	}
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	frame = append(frame, mask...)
	for i, b := range body {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// readUntil collects payloads until the needle appears.
func (c *wsClient) readUntil(t *testing.T, needle string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var seen strings.Builder

	for time.Now().Before(deadline) {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		var header [2]byte
		if _, err := io.ReadFull(c.conn, header[:]); err != nil {
			break
		}
		length := uint64(header[1] & 0x7F)
		switch length {
		case 126:
			var extended [2]byte
			if _, err := io.ReadFull(c.conn, extended[:]); err != nil {
				break
			}
			length = uint64(binary.BigEndian.Uint16(extended[:]))
		case 127:
			var extended [8]byte
			if _, err := io.ReadFull(c.conn, extended[:]); err != nil {
				break
			}
			length = binary.BigEndian.Uint64(extended[:])
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			break
		}
		// Server frames must never be masked.
		if header[1]&0x80 != 0 {
			t.Fatal("the server masked a frame; only clients mask")
		}
		seen.Write(payload)
		if strings.Contains(seen.String(), needle) {
			return seen.String()
		}
	}
	return seen.String()
}

func (c *wsClient) Close() { c.conn.Close() }

func TestShellRunsCommandsOnTheMachine(t *testing.T) {
	f := newFixtureWithShell(t)
	fork := f.forkWithMachine(t)

	client, resp := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell?cols=80&rows=24", nil)
	defer client.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got == "" {
		t.Fatal("no Sec-WebSocket-Accept in the handshake")
	}

	client.send(t, shellMessage{Type: "input", Data: "echo shell-is-live\n"})
	if got := client.readUntil(t, "shell-is-live", 10*time.Second); !strings.Contains(got, "shell-is-live") {
		t.Fatalf("the shell produced no output: %q", got)
	}
}

func TestShellIsARealTerminal(t *testing.T) {
	f := newFixtureWithShell(t)
	fork := f.forkWithMachine(t)

	client, _ := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell", nil)
	defer client.Close()

	// The point of a terminal rather than pipes: an operator dropping in
	// should get what they would get over SSH.
	client.send(t, shellMessage{Type: "input", Data: "test -t 0 && echo HAS-TTY\n"})
	if got := client.readUntil(t, "HAS-TTY", 10*time.Second); !strings.Contains(got, "HAS-TTY") {
		t.Fatalf("the shell is not attached to a terminal: %q", got)
	}
}

func TestShellResizeReachesTheTerminal(t *testing.T) {
	f := newFixtureWithShell(t)
	fork := f.forkWithMachine(t)

	client, _ := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell?cols=80&rows=24", nil)
	defer client.Close()

	client.send(t, shellMessage{Type: "resize", Cols: 132, Rows: 43})
	client.send(t, shellMessage{Type: "input", Data: "stty size\n"})
	if got := client.readUntil(t, "43 132", 10*time.Second); !strings.Contains(got, "43 132") {
		t.Fatalf("resize did not reach the terminal: %q", got)
	}
}

func TestShellIsRefusedFromAnotherOrigin(t *testing.T) {
	f := newFixtureWithShell(t)
	fork := f.forkWithMachine(t)

	// The control plane has no authentication of its own. Without this check
	// any page the operator visits could open a shell on their machine.
	_, resp := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell",
		map[string]string{"Origin": "https://evil.example.com"})

	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("a cross-origin websocket was accepted")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestShellAcceptsItsOwnOrigin(t *testing.T) {
	f := newFixtureWithShell(t)
	fork := f.forkWithMachine(t)

	_, resp := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell",
		map[string]string{"Origin": f.server.URL})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want the same-origin socket accepted", resp.StatusCode)
	}
}

func TestShellNeedsAMachine(t *testing.T) {
	f := newFixtureWithShell(t)
	_, forks := f.startTask(t, "add ratings")

	// Nothing has booted yet, so there is nothing to attach to.
	_, resp := dialShell(t, f, "/v1/forks/"+forks[0].ID+"/shell", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 before a machine exists", resp.StatusCode)
	}
}

func TestShellIsUnavailableWithoutAnInteractiveDriver(t *testing.T) {
	f := newFixture(t, planJSON(nil, oneWorkstream()))
	fork := f.forkWithMachine(t)

	// The fixture's default driver cannot open terminals; that must be said
	// plainly rather than failing an upgrade the browser cannot interpret.
	_, resp := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestOpeningAShellIsAudited(t *testing.T) {
	f := newFixtureWithShell(t)
	fork := f.forkWithMachine(t)

	client, _ := dialShell(t, f, "/v1/forks/"+fork.ID+"/shell", nil)
	client.send(t, shellMessage{Type: "input", Data: "echo hi\n"})
	client.readUntil(t, "hi", 10*time.Second)
	client.Close()

	var body struct {
		Events []*domain.Event `json:"events"`
	}
	f.call(t, http.MethodGet, "/v1/audit?fork="+fork.ID+"&limit=50", nil, &body)

	var found *domain.Event
	for _, event := range body.Events {
		if event.Type == domain.EventShellOpened {
			found = event
		}
	}
	if found == nil {
		t.Fatal("opening a shell was not recorded")
	}
	// From a shell a person can change anything the agent can, so the trail
	// must show it was them.
	if found.Actor != domain.ActorUser {
		t.Fatalf("shell attributed to %q, want the user", found.Actor)
	}
}
