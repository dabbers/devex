package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/claudeauth"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// tokenRecorder is a driver that remembers the Claude token each agent run
// carried and then fails it, which is all a conversation test needs to see
// whose subscription a turn would have spent.
type tokenRecorder struct {
	unreachableDriver
	mu     sync.Mutex
	tokens []string
}

func (d *tokenRecorder) Exec(_ context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tokens = append(d.tokens, cmd.Env[agent.TokenEnv])
	return nil, errors.New("recorded")
}

func (d *tokenRecorder) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.tokens...)
}

// rawCall makes a request and returns the body unparsed, so a test can check
// that a value appears nowhere in it rather than only not in one field.
func rawCall(t *testing.T, f *fixture, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(rec, req)
	raw, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(raw)
}

// settledReply waits for a workspace's latest reply to stop running.
func settledReply(t *testing.T, f *fixture, workspaceID string) domain.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := f.store.ListMessages(f.ctx, workspaceID, 50)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if n := len(messages); n > 0 && messages[n-1].State != domain.MessageRunning {
			return *messages[n-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the reply never settled")
	return domain.Message{}
}

// TestAWorkspaceTurnIsPaidForByItsOwnerAndNobodyElse is the end-to-end form
// of the bring-your-own-subscription rule, through the API a customer uses:
// with no token of their own a turn is refused and says why; with one stored
// the turn carries exactly that token -- not another customer's, which is
// sitting in the same vault, and not the daemon's, which is in the process
// environment.
func TestAWorkspaceTurnIsPaidForByItsOwnerAndNobodyElse(t *testing.T) {
	const (
		ownerToken  = "sk-ant-oat01-owner-token"
		otherToken  = "sk-ant-oat01-other-customer"
		daemonToken = "sk-ant-oat01-daemon-token"
	)
	t.Setenv(agent.TokenEnv, daemonToken)

	f := newFixture(t)
	withProject(t, f)
	ws := runningWorkspace(t, f)

	other, err := f.store.EnsureUser(f.ctx, "other@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if err := f.api.vault.SetCredential(f.ctx, other.ID, claudeauth.Kind, otherToken); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}

	// The resolver main.go builds, with the fallback off -- the default.
	resolver, err := claudeauth.New(claudeauth.Options{Vault: f.api.vault})
	if err != nil {
		t.Fatalf("claudeauth.New: %v", err)
	}
	driver := &tokenRecorder{}
	f.api.agent = agent.New(driver, resolver, agent.Config{})
	f.api.claude = resolver

	// No token yet: the turn is refused before the agent starts, and the
	// customer is told what to do.
	if status := f.call(t, "POST", "/v1/workspaces/"+ws.ID+"/messages",
		map[string]any{"content": "tighten the spacing"}, nil); status != http.StatusAccepted {
		t.Fatalf("send: %d", status)
	}
	reply := settledReply(t, f, ws.ID)
	if reply.State != domain.MessageFailed || !strings.Contains(reply.Content, "no Claude token") {
		t.Fatalf("a turn with no token was not refused plainly: %q %q", reply.State, reply.Content)
	}
	if seen := driver.seen(); len(seen) != 0 {
		t.Fatalf("the agent was started with no token of the owner's: %q", seen)
	}

	// Stored, with the newline a paste brings. The response says it is set
	// and never repeats it.
	status, body := rawCall(t, f, "PUT", "/v1/claude", `{"token":"`+ownerToken+`\n"}`)
	if status != http.StatusOK {
		t.Fatalf("put: %d %s", status, body)
	}
	if strings.Contains(body, ownerToken) {
		t.Fatalf("storing the token echoed it: %s", body)
	}
	var stored struct {
		Claude claudeauth.Status `json:"claude"`
	}
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !stored.Claude.Set || stored.Claude.Source != claudeauth.SourceAccount {
		t.Fatalf("status after storing = %+v", stored.Claude)
	}
	if _, body := rawCall(t, f, "GET", "/v1/claude", ""); strings.Contains(body, ownerToken) {
		t.Fatalf("the status returned the token: %s", body)
	}

	if status := f.call(t, "POST", "/v1/workspaces/"+ws.ID+"/messages",
		map[string]any{"content": "tighten the spacing"}, nil); status != http.StatusAccepted {
		t.Fatalf("send: %d", status)
	}
	settledReply(t, f, ws.ID)
	seen := driver.seen()
	if len(seen) != 1 || seen[0] != ownerToken {
		t.Fatalf("the owner's turn carried %q, want only their own token", seen)
	}

	// Nothing about it reached the audit trail but the fact.
	events, err := f.store.ListEvents(f.ctx, store.EventFilter{UserID: f.owner.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		if strings.Contains(string(raw), ownerToken) {
			t.Fatalf("the audit trail holds the token: %s", raw)
		}
	}

	// Removed, the next turn is refused again rather than falling to anyone.
	if status, body := rawCall(t, f, "DELETE", "/v1/claude", ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", status, body)
	}
	if status := f.call(t, "POST", "/v1/workspaces/"+ws.ID+"/messages",
		map[string]any{"content": "and again"}, nil); status != http.StatusAccepted {
		t.Fatalf("send: %d", status)
	}
	if reply := settledReply(t, f, ws.ID); reply.State != domain.MessageFailed {
		t.Fatalf("a turn after removing the token was not refused: %q", reply.State)
	}
	if seen := driver.seen(); len(seen) != 1 {
		t.Fatalf("a run started after the token was removed: %q", seen)
	}
}

func TestABadTokenIsRefusedWithoutBeingRepeated(t *testing.T) {
	f := newFixture(t)
	for name, token := range map[string]string{
		"empty":           "   ",
		"inner newline":   "sk-ant-oat01-abc\\ndef",
		"inner space":     "sk-ant-oat01 abc",
		"absurdly long":   strings.Repeat("x", 5000),
		"control charact": "sk-ant-\\u0007bell",
	} {
		t.Run(name, func(t *testing.T) {
			status, body := rawCall(t, f, "PUT", "/v1/claude", `{"token":"`+token+`"}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, body)
			}
			if len(token) >= 12 && strings.Contains(body, token[:12]) {
				t.Fatalf("the refusal repeats the token: %s", body)
			}
		})
	}
	set, _, err := f.api.vault.CredentialSet(f.ctx, f.owner.ID, claudeauth.Kind)
	if err != nil || set {
		t.Fatalf("a refused token was stored anyway (set=%v, err=%v)", set, err)
	}
}
