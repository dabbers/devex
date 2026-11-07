package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
)

func seedConversationWorkspace(t *testing.T, s *Store) (*domain.User, *domain.Workspace) {
	t.Helper()
	u, r, p := seed(t, s, t.Context())
	w := seedWorkspace(t, s, u, r, p, "cart")
	return u, w
}

func TestStartingATurnRecordsBothSides(t *testing.T) {
	s, ctx := newTestStore(t)
	u, w := seedConversationWorkspace(t, s)

	reply, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "make the spacing tighter",
	})
	if err != nil {
		t.Fatalf("StartAgentTurn: %v", err)
	}
	if reply.State != domain.MessageRunning {
		t.Fatalf("reply state = %q, want running", reply.State)
	}

	messages, err := s.ListMessages(ctx, w.ID, 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want the prompt and the reply", len(messages))
	}
	// Order matters: a reply that sorts before its prompt reads as the agent
	// answering before it was asked.
	if messages[0].Role != domain.MessageFromUser || messages[1].Role != domain.MessageFromAgent {
		t.Fatalf("messages are out of order: %q then %q", messages[0].Role, messages[1].Role)
	}
	// A user's message is complete the moment it is said.
	if messages[0].State != domain.MessageDone {
		t.Fatalf("the prompt is %q, want done", messages[0].State)
	}
}

// TestTwoTurnsCannotRunAtOnce: two runs on one machine would be changing the
// same files underneath each other.
func TestTwoTurnsCannotRunAtOnce(t *testing.T) {
	s, ctx := newTestStore(t)
	u, w := seedConversationWorkspace(t, s)

	if _, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "first",
	}); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	_, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "second",
	})
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("second turn error = %v, want ErrAgentBusy", err)
	}

	// And nothing was written for the refused turn.
	messages, err := s.ListMessages(ctx, w.ID, 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("a refused turn still wrote messages: %d", len(messages))
	}
}

func TestAnotherWorkspaceIsNotBlocked(t *testing.T) {
	s, ctx := newTestStore(t)
	u, r, p := seed(t, s, ctx)
	first := seedWorkspace(t, s, u, r, p, "one")
	second := seedWorkspace(t, s, u, r, p, "two")

	if _, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: first.ID, UserID: u.ID, Content: "busy",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Separate machines, so they do not contend.
	if _, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: second.ID, UserID: u.ID, Content: "also busy",
	}); err != nil {
		t.Fatalf("a second workspace was blocked by the first: %v", err)
	}
}

func TestFinishingATurnFreesTheWorkspace(t *testing.T) {
	s, ctx := newTestStore(t)
	u, w := seedConversationWorkspace(t, s)

	reply, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "first",
	})
	if err != nil {
		t.Fatalf("StartAgentTurn: %v", err)
	}
	reply.Content = "done"
	reply.State = domain.MessageDone
	reply.Usage = domain.Usage{CostUSD: 0.4, InputTokens: 900}
	if err := s.FinishMessage(ctx, reply); err != nil {
		t.Fatalf("FinishMessage: %v", err)
	}

	if _, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "second",
	}); err != nil {
		t.Fatalf("the workspace was still busy after the turn finished: %v", err)
	}

	messages, err := s.ListMessages(ctx, w.ID, 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if messages[1].Content != "done" || messages[1].Usage.CostUSD != 0.4 {
		t.Fatalf("the finished reply lost its content or usage: %+v", messages[1])
	}
}

// TestARestartAbandonsRunningTurns: a run lives in a goroutine, so a restart
// ends it without ending the row. Left running, the workspace shows an agent
// working forever and refuses every later message.
func TestARestartAbandonsRunningTurns(t *testing.T) {
	s, ctx := newTestStore(t)
	u, w := seedConversationWorkspace(t, s)

	if _, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "in flight",
	}); err != nil {
		t.Fatalf("StartAgentTurn: %v", err)
	}

	abandoned, err := s.AbandonRunningMessages(ctx, "the control plane restarted")
	if err != nil {
		t.Fatalf("AbandonRunningMessages: %v", err)
	}
	if abandoned != 1 {
		t.Fatalf("abandoned %d turns, want 1", abandoned)
	}

	messages, err := s.ListMessages(ctx, w.ID, 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if messages[1].State != domain.MessageFailed {
		t.Fatalf("the orphaned turn is %q, want failed", messages[1].State)
	}
	// Saying why, so it does not read as the agent failing at the work.
	if !strings.Contains(messages[1].StateReason, "restarted") {
		t.Fatalf("reason %q does not explain itself", messages[1].StateReason)
	}
	if _, err := s.StartAgentTurn(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Content: "after the restart",
	}); err != nil {
		t.Fatalf("the workspace is still blocked after cleanup: %v", err)
	}
}

func TestAnUnknownRoleOrStateIsRefused(t *testing.T) {
	s, ctx := newTestStore(t)
	u, w := seedConversationWorkspace(t, s)

	if err := s.AppendMessage(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Role: domain.MessageRole("system"), Content: "x",
	}); err == nil {
		t.Fatal("an unknown role was stored")
	}
	if err := s.AppendMessage(ctx, &domain.Message{
		WorkspaceID: w.ID, UserID: u.ID, Role: domain.MessageFromAgent,
		State: domain.MessageState("thinking"),
	}); err == nil {
		t.Fatal("an unknown state was stored")
	}
}
