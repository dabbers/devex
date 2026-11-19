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
	"github.com/dabbers/devex/internal/agentauth"
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

func decodeAgentStatus(t *testing.T, body string) agentCredentialResponse {
	t.Helper()
	var response agentCredentialResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return response
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

// TestAWorkspaceTurnIsDrivenByItsOwnersSubscriptionAndNobodyElses is the
// end-to-end form of the bring-your-own-subscription rule, through the API a
// customer uses: with nothing stored a turn is refused and says why; with a
// Codex credential it is refused as not supported yet; with a Claude token
// the turn carries exactly that token -- not another customer's, which is
// sitting in the same vault, and not the daemon's, which is in the process
// environment.
func TestAWorkspaceTurnIsDrivenByItsOwnersSubscriptionAndNobodyElses(t *testing.T) {
	const (
		ownerToken  = "sk-ant-oat01-owner-token"
		codexToken  = "codex-owner-credential"
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
	if err := f.api.vault.SetCredential(f.ctx, other.ID, agentauth.Kind, "claude", otherToken); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}

	// The resolver main.go builds, with the fallback off -- the default.
	resolver, err := agentauth.New(agentauth.Options{Vault: f.api.vault})
	if err != nil {
		t.Fatalf("agentauth.New: %v", err)
	}
	driver := &tokenRecorder{}
	f.api.agent = agent.New(driver, resolver, agent.Config{})
	f.api.agentCreds = resolver

	send := func(content string) domain.Message {
		t.Helper()
		if status := f.call(t, "POST", "/v1/workspaces/"+ws.ID+"/messages",
			map[string]any{"content": content}, nil); status != http.StatusAccepted {
			t.Fatalf("send: %d", status)
		}
		return settledReply(t, f, ws.ID)
	}

	// Nothing stored: the panel is told so, and a turn is refused before the
	// agent starts, with a message saying what to do.
	_, body := rawCall(t, f, "GET", "/v1/account/agent", "")
	if got := decodeAgentStatus(t, body); got.Agent.Set || got.Agent.Source != agentauth.SourceNone ||
		got.Agent.Blocked == "" || len(got.Providers) != 3 {
		t.Fatalf("status with nothing stored = %s", body)
	}
	if reply := send("tighten the spacing"); reply.State != domain.MessageFailed ||
		!strings.Contains(reply.Content, "no agent subscription credential") {
		t.Fatalf("a turn with nothing stored was not refused plainly: %q %q", reply.State, reply.Content)
	}

	// A Codex credential is stored and recognised, and its runs are refused
	// as not supported yet rather than passed to Claude Code.
	status, body := rawCall(t, f, "PUT", "/v1/account/agent", `{"provider":"codex","credential":"`+codexToken+`"}`)
	if status != http.StatusOK || strings.Contains(body, codexToken) {
		t.Fatalf("put codex: %d %s", status, body)
	}
	if got := decodeAgentStatus(t, body); got.Agent.Provider != domain.ProviderCodex || !got.Agent.Set ||
		got.Agent.Supported || !strings.Contains(got.Agent.Blocked, "Codex") {
		t.Fatalf("status with a codex credential = %s", body)
	}
	if reply := send("tighten the spacing"); reply.State != domain.MessageFailed ||
		!strings.Contains(reply.Content, "Codex yet") {
		t.Fatalf("a codex turn was not refused as unsupported: %q %q", reply.State, reply.Content)
	}
	if seen := driver.seen(); len(seen) != 0 {
		t.Fatalf("the agent was started without a Claude token of the owner's: %q", seen)
	}

	// Replaced by a Claude token, with the newline a paste brings. The
	// response says it is set and never repeats it.
	status, body = rawCall(t, f, "PUT", "/v1/account/agent", `{"provider":"claude","credential":"`+ownerToken+`\n"}`)
	if status != http.StatusOK {
		t.Fatalf("put: %d %s", status, body)
	}
	if strings.Contains(body, ownerToken) {
		t.Fatalf("storing the credential echoed it: %s", body)
	}
	got := decodeAgentStatus(t, body)
	if !got.Agent.Set || got.Agent.Provider != domain.ProviderClaude || !got.Agent.Supported ||
		got.Agent.Source != agentauth.SourceAccount || got.Agent.Blocked != "" || got.Agent.UpdatedAt == nil {
		t.Fatalf("status after storing = %s", body)
	}
	if _, body := rawCall(t, f, "GET", "/v1/account/agent", ""); strings.Contains(body, ownerToken) {
		t.Fatalf("the status returned the credential: %s", body)
	}

	send("tighten the spacing")
	seen := driver.seen()
	if len(seen) != 1 || seen[0] != ownerToken {
		t.Fatalf("the owner's turn carried %q, want only their own token", seen)
	}

	// Nothing about it reached the audit trail but the fact and the provider.
	events, err := f.store.ListEvents(f.ctx, store.EventFilter{UserID: f.owner.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		if strings.Contains(string(raw), ownerToken) || strings.Contains(string(raw), codexToken) {
			t.Fatalf("the audit trail holds a credential: %s", raw)
		}
	}

	// Removed, the next turn is refused again rather than falling to anyone.
	if status, body := rawCall(t, f, "DELETE", "/v1/account/agent", ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", status, body)
	}
	if reply := send("and again"); reply.State != domain.MessageFailed {
		t.Fatalf("a turn after removing the credential was not refused: %q", reply.State)
	}
	if seen := driver.seen(); len(seen) != 1 {
		t.Fatalf("a run started after the credential was removed: %q", seen)
	}
}

func TestABadCredentialIsRefusedWithoutBeingRepeated(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]string{
		"empty":             `{"provider":"claude","credential":"   "}`,
		"no provider":       `{"credential":"sk-ant-oat01-abcdefgh"}`,
		"unknown provider":  `{"provider":"gemini","credential":"sk-ant-oat01-abcdefgh"}`,
		"inner newline":     `{"provider":"claude","credential":"sk-ant-oat01-abc\ndef"}`,
		"inner space":       `{"provider":"claude","credential":"sk-ant-oat01 abcdefg"}`,
		"absurdly long":     `{"provider":"claude","credential":"` + strings.Repeat("x", 5000) + `"}`,
		"control character": `{"provider":"claude","credential":"sk-ant-oat01-\u0007bell"}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, response := rawCall(t, f, "PUT", "/v1/account/agent", body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, response)
			}
			if strings.Contains(response, "sk-ant-oat01") || strings.Contains(response, "xxxxxxxxxxxx") {
				t.Fatalf("the refusal repeats the credential: %s", response)
			}
		})
	}
	info, err := f.api.vault.CredentialSet(f.ctx, f.owner.ID, agentauth.Kind)
	if err != nil || info != nil {
		t.Fatalf("a refused credential was stored anyway (%+v, err=%v)", info, err)
	}
}
