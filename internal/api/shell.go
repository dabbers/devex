package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// shellMessage is what the browser sends up the socket.
//
// Output travels the other way as raw binary, because a terminal's bytes are
// not necessarily valid UTF-8 and a text frame must be.
type shellMessage struct {
	// Type is "input" for keystrokes or "resize" for a window change.
	Type string `json:"t"`
	// Data carries keystrokes for an input message.
	Data string `json:"d,omitempty"`
	// Cols and Rows carry the new size for a resize message.
	Cols uint16 `json:"c,omitempty"`
	Rows uint16 `json:"r,omitempty"`
}

// shell attaches a browser terminal to a sub-task's machine.
//
// This is the workspace shell the requirements call for: the same machine the
// agents are working on, so an operator can see what a stuck agent sees.
func (s *Server) shell(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	fork, err := s.store.GetFork(ctx, r.PathValue("fork"))
	if err != nil {
		writeError(w, err)
		return
	}
	if fork.InstanceID == "" {
		writeStatus(w, http.StatusConflict, "this sub-task has no machine yet")
		return
	}

	// Not every driver can open a terminal, so degrade with an explanation
	// rather than a failed upgrade the browser cannot interpret.
	interactive, ok := s.driver.(vm.Interactive)
	if !ok {
		writeStatus(w, http.StatusNotImplemented, "this VM driver cannot open a shell")
		return
	}

	// Upgrade before opening the session: a refused handshake should not leave
	// a shell running on the machine.
	conn, err := upgrade(w, r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	defer conn.Close()

	session, err := interactive.OpenSession(ctx, fork.InstanceID, vm.SessionSpec{
		Cols: uint16(intParam(r, "cols", 80)),
		Rows: uint16(intParam(r, "rows", 24)),
	})
	if err != nil {
		//nolint:errcheck // the socket is closing either way
		conn.writeText("dabberz: could not open a shell: " + err.Error() + "\r\n")
		return
	}
	defer session.Close()

	// Opening a shell on a machine an agent is working in is worth recording:
	// from here a person can change anything the agent can.
	s.audit(ctx, &domain.Event{
		RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Type:    domain.EventShellOpened,
		Message: "opened a shell on " + fork.Name,
		Data:    map[string]any{"instance": fork.InstanceID},
	})

	// Pump output until the session ends, then drop the socket so the reader
	// below stops waiting on a terminal nobody is writing to.
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, 16<<10)
		for {
			n, err := session.Read(buf)
			if n > 0 {
				if writeErr := conn.writeBinary(buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					//nolint:errcheck // best effort on a dying socket
					conn.writeText("\r\ndabberz: " + err.Error() + "\r\n")
				}
				return
			}
		}
	}()

reading:
	for {
		opcode, payload, err := conn.readMessage()
		if err != nil {
			break
		}
		if opcode != opText {
			continue
		}

		var message shellMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			continue
		}
		switch message.Type {
		case "input":
			if _, err := io.WriteString(session, message.Data); err != nil {
				// The label matters: a bare break here leaves the switch, not
				// the loop, and the session would go on silently swallowing
				// every keystroke after the terminal had gone.
				break reading
			}
		case "resize":
			//nolint:errcheck // a rejected resize is not worth ending the session
			session.Resize(message.Cols, message.Rows)
		}
	}

	// Closing the session ends the shell and everything it started, which is
	// what stops a closed browser tab leaving work running on the machine.
	session.Close()
	<-outputDone
}
