package domain

import "testing"

// TestResumedRunsAreNotCountedTwice is the accounting bug this type exists for.
// A resumed run reports the whole conversation's total; adding that to a
// running total counts every earlier turn again, and the totals drive the cost
// and token tripwires, so a fork would be stopped for spending it never did.
func TestResumedRunsAreNotCountedTwice(t *testing.T) {
	session := &AgentSession{}
	var budget Usage

	// First run: the conversation has cost $1.
	budget.Add(session.Turn("sess_1", Usage{CostUSD: 1, InputTokens: 100, OutputTokens: 50}))
	if budget.CostUSD != 1 {
		t.Fatalf("after one run the budget is %v, want 1", budget.CostUSD)
	}

	// Resumed: the provider reports $3 for the conversation so far, which
	// means this turn cost $2.
	budget.Add(session.Turn("sess_1", Usage{CostUSD: 3, InputTokens: 260, OutputTokens: 140}))
	if budget.CostUSD != 3 {
		t.Fatalf("budget is %v, want the conversation's actual 3", budget.CostUSD)
	}
	if budget.InputTokens != 260 || budget.OutputTokens != 140 {
		t.Fatalf("tokens = %d/%d, want 260/140", budget.InputTokens, budget.OutputTokens)
	}

	// A third turn, to be sure the subtraction keeps working.
	budget.Add(session.Turn("sess_1", Usage{CostUSD: 3.5, InputTokens: 300, OutputTokens: 160}))
	if budget.CostUSD != 3.5 {
		t.Fatalf("budget is %v, want 3.5", budget.CostUSD)
	}
}

// TestANewConversationStartsItsOwnAccounting: a changed identifier means the
// figures belong to a different conversation, so nothing is accounted yet.
func TestANewConversationStartsItsOwnAccounting(t *testing.T) {
	session := &AgentSession{}
	var budget Usage

	budget.Add(session.Turn("sess_1", Usage{CostUSD: 5, InputTokens: 500}))

	// The machine was rebuilt and the provider started a fresh conversation.
	// Subtracting the old total would produce a negative turn.
	turn := session.Turn("sess_2", Usage{CostUSD: 1, InputTokens: 80})
	if turn.CostUSD != 1 || turn.InputTokens != 80 {
		t.Fatalf("turn = %+v, want the new conversation's own figures", turn)
	}
	budget.Add(turn)
	if budget.CostUSD != 6 {
		t.Fatalf("budget is %v, want 6", budget.CostUSD)
	}
	if session.ID != "sess_2" {
		t.Fatalf("session id = %q, want sess_2", session.ID)
	}
}

// TestFiguresGoingBackwardsResetAccounting guards against a negative turn,
// which would credit a fork budget it never earned and let it run past a
// tripwire.
func TestFiguresGoingBackwardsResetAccounting(t *testing.T) {
	session := &AgentSession{ID: "sess_1", Accounted: Usage{CostUSD: 10, InputTokens: 1000}}

	turn := session.Turn("sess_1", Usage{CostUSD: 2, InputTokens: 200})
	if turn.CostUSD < 0 || turn.InputTokens < 0 {
		t.Fatalf("turn went negative: %+v", turn)
	}
	if turn.CostUSD != 2 {
		t.Fatalf("turn cost = %v, want the reported 2", turn.CostUSD)
	}
}

// TestWallTimeIsPerRun: it is measured by dabberz rather than reported by the
// agent, so it is already this run's and must not be differenced.
func TestWallTimeIsPerRun(t *testing.T) {
	session := &AgentSession{}
	first := session.Turn("sess_1", Usage{CostUSD: 1, Wall: 60})
	second := session.Turn("sess_1", Usage{CostUSD: 2, Wall: 90})
	if first.Wall != 60 || second.Wall != 90 {
		t.Fatalf("wall = %v then %v, want the measured values", first.Wall, second.Wall)
	}
}

func TestStartedReportsWhetherThereIsAConversation(t *testing.T) {
	var absent *AgentSession
	if absent.Started() {
		t.Fatal("a nil session reported a conversation")
	}
	if (&AgentSession{}).Started() {
		t.Fatal("an empty session reported a conversation")
	}
	if !(&AgentSession{ID: "sess_1"}).Started() {
		t.Fatal("a session with an id reported none")
	}
}
