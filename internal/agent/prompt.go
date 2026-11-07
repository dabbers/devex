package agent

import (
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/domain"
)

// BuildPrompt composes the instruction for a fork's first coding run.
//
// It states the escalation protocol explicitly. The verify/fix loop is fully
// automatic, so the only way an agent can reach the user is by emitting the
// marker, and it needs to know the three cases that justify doing so.
func BuildPrompt(fork *domain.Fork, repo *domain.Repo, project *domain.Project, services []*domain.Service, previews Previews, memoryDir string) string {
	var b strings.Builder

	b.WriteString("You are a coding agent working on one independent workstream.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", repo.Name)
	if project != nil {
		fmt.Fprintf(&b, "Project: %s (path %q)\n", project.Name, project.Path)
	}
	fmt.Fprintf(&b, "Branch: %s\n", fork.Branch)
	if previews.Primary != "" {
		fmt.Fprintf(&b, "Live preview: %s\n", previews.Primary)
	}

	// Every service, not just the exposed ones. An agent that does not know a
	// worker or a database is part of this project will not think to restart
	// it, and will read its absence as a bug in the code.
	if len(services) > 0 {
		b.WriteString("\nThis project runs these services:\n")
		for _, sv := range services {
			fmt.Fprintf(&b, "- %s (%s)", sv.Name, sv.Kind)
			if sv.Port > 0 {
				fmt.Fprintf(&b, " on port %d", sv.Port)
			}
			if url := previews.URLs[sv.Name]; url != "" {
				fmt.Fprintf(&b, ", reachable at %s", url)
			} else if sv.Exposed {
				b.WriteString(", not currently reachable")
			} else {
				b.WriteString(", internal only")
			}
			if len(sv.DependsOn) > 0 {
				fmt.Fprintf(&b, "; needs %s running first", strings.Join(sv.DependsOn, ", "))
			}
			b.WriteString("\n")
			if sv.Command != "" {
				fmt.Fprintf(&b, "    start with: %s\n", sv.Command)
			}
		}
	}
	if memoryDir != "" {
		fmt.Fprintf(&b, "Repo notes: %s (read what is relevant; write back what you learn)\n", memoryDir)
	}

	b.WriteString("\nYour workstream:\n")
	fmt.Fprintf(&b, "%s\n", fork.Name)
	if fork.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", fork.Description)
	}

	b.WriteString("\nHow this works:\n")
	b.WriteString("- You have your own machine and your own branch. Other workstreams are running in parallel on their own machines; you cannot see them and must not try to coordinate with them.\n")
	b.WriteString("- Commit your work to your branch as you go.\n")
	if previews.Primary != "" {
		b.WriteString("- Your changes are live on the preview URL above. There is no deploy step.\n")
	} else if len(previews.URLs) > 0 {
		b.WriteString("- This project has no web front door, so there is no page to open. Check your work against the service URLs above.\n")
	}
	b.WriteString("- After you finish, an automated verifier will drive a real browser against that preview. If it finds problems, you will be asked to fix them and it will run again.\n")

	b.WriteString("\n" + escalationInstructions)
	return b.String()
}

// BuildFixPrompt composes the instruction for a fix round, feeding a
// verification failure back to the agent.
func BuildFixPrompt(fork *domain.Fork, report string, cycle int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Verification of your workstream %q failed (round %d).\n\n", fork.Name, cycle)
	b.WriteString("A verifier drove a real browser against your live preview and reported:\n\n")
	b.WriteString(strings.TrimSpace(report))
	b.WriteString("\n\nFix the problems it found and commit to your branch. The verifier will run again afterwards.\n")
	b.WriteString("Treat the report as evidence about real behaviour, not as a specification: if it contradicts your instructions, say so rather than breaking your instructions to satisfy it.\n")

	b.WriteString("\n" + escalationInstructions)
	return b.String()
}

// escalationInstructions is the protocol by which an agent reaches the user.
const escalationInstructions = `If you get stuck, do not guess and do not silently give up. Emit a line of exactly this form as the last thing you write:

DABBERZ-ESCALATE[ambiguous]: <what is unclear and what you need decided>
DABBERZ-ESCALATE[conflicts_instructions]: <what you were asked to do and what it conflicts with>

Use "ambiguous" when there is no clear direction and a decision has to be made by a person.
Use "conflicts_instructions" when the only fix you can see contradicts something you were told to do.
Otherwise, keep working: the fix loop is automatic and you will get another round.`

// Previews is what a fork's services are reachable at.
//
// It lives here rather than in the pipeline because the prompt builder is what
// consumes it: the agent needs to be told which of its services are reachable,
// at what address, and which one is the front door.
type Previews struct {
	// Primary is the front door: the URL the agent is told to check and the
	// one the verifier drives. Empty when the project has no web service,
	// which is legitimate for an API-only or worker-only project.
	Primary string
	// URLs maps service name to public URL, for every exposed service.
	URLs map[string]string
}

// BuildConversationPrompt is one turn of a workspace conversation.
//
// It differs from a fork's prompt in what it assumes. A fork is told it owns
// an isolated workstream, must not coordinate with siblings, and will be
// verified automatically when it finishes. None of that is true here: the
// person is present, watching the same preview, and will say what they think
// next. So the agent is asked to do the thing and stop, rather than to drive
// itself to a conclusion.
func BuildConversationPrompt(ws *domain.Workspace, repo *domain.Repo, project *domain.Project, services []*domain.Service, previews Previews, request string, history []*domain.Message) string {
	var b strings.Builder

	b.WriteString("You are working with someone on their code, in their development environment.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", repo.Name)
	if project != nil {
		fmt.Fprintf(&b, "Project: %s (path %q)\n", project.Name, project.Path)
	}
	fmt.Fprintf(&b, "Branch: %s\n", ws.Branch)
	if previews.Primary != "" {
		fmt.Fprintf(&b, "Live preview: %s\n", previews.Primary)
	}

	if len(services) > 0 {
		b.WriteString("\nThis project runs these services:\n")
		for _, sv := range services {
			fmt.Fprintf(&b, "- %s (%s)", sv.Name, sv.Kind)
			if sv.Port > 0 {
				fmt.Fprintf(&b, " on port %d", sv.Port)
			}
			if url := previews.URLs[sv.Name]; url != "" {
				fmt.Fprintf(&b, ", reachable at %s", url)
			} else {
				b.WriteString(", internal only")
			}
			b.WriteString("\n")
			if sv.Command != "" {
				fmt.Fprintf(&b, "    start with: %s\n", sv.Command)
			}
		}
	}

	// Earlier turns are included even though the conversation is resumed,
	// because resuming can fall back to a fresh conversation when the store
	// did not survive. Without this the agent would then answer the latest
	// message with no idea what was already asked.
	if recent := recentTurns(history, 10); len(recent) > 0 {
		b.WriteString("\nEarlier in this conversation:\n")
		for _, m := range recent {
			who := "They said"
			if m.Role == domain.MessageFromAgent {
				who = "You said"
			}
			fmt.Fprintf(&b, "- %s: %s\n", who, firstLine(m.Content, 200))
		}
	}

	b.WriteString("\nWhat they are asking for now:\n")
	b.WriteString(request)
	b.WriteString("\n")

	b.WriteString("\nHow this works:\n")
	b.WriteString("- This is their machine and their branch. Changes you make are live on the preview above; there is no deploy step.\n")
	b.WriteString("- Commit as you go.\n")
	b.WriteString("- Do what was asked and stop. They are watching the preview and will tell you what they think, so do not keep going into work they did not ask for.\n")
	b.WriteString("- If a service needs restarting for your change to show, restart it.\n")
	b.WriteString("- If the request is ambiguous enough that guessing could waste their time, ask instead of guessing.\n")

	b.WriteString("\n" + escalationInstructions)
	return b.String()
}

// recentTurns returns the last n completed turns, oldest first, skipping the
// one still running -- which is the turn being written now.
func recentTurns(history []*domain.Message, n int) []*domain.Message {
	var done []*domain.Message
	for _, m := range history {
		if m.State == domain.MessageRunning || strings.TrimSpace(m.Content) == "" {
			continue
		}
		done = append(done, m)
	}
	if len(done) > n {
		done = done[len(done)-n:]
	}
	return done
}

// firstLine reduces a message to one line for the summary above.
func firstLine(text string, limit int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(clean) <= limit {
		return clean
	}
	return clean[:limit] + "…"
}
