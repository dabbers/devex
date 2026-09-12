package merge

import (
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/domain"
)

// reviewPrompt asks the agent to gate a change before it lands.
func reviewPrompt(fork *domain.Fork, target string) string {
	var b strings.Builder

	b.WriteString("You are reviewing a completed workstream before it lands on a shared branch.\n\n")
	fmt.Fprintf(&b, "Workstream: %s\n", fork.Name)
	if fork.Description != "" {
		fmt.Fprintf(&b, "It was asked to: %s\n", fork.Description)
	}
	fmt.Fprintf(&b, "Branch: %s\nTarget: %s\n\n", fork.Branch, target)

	b.WriteString("Review the diff between the target branch and this branch. Judge whether it is fit to land:\n")
	b.WriteString("- Does it actually do what the workstream was asked to do?\n")
	b.WriteString("- Is it correct? Look for bugs, unhandled errors and broken edge cases.\n")
	b.WriteString("- Does it match the conventions already used in this repository?\n")
	b.WriteString("- Did it leave behind debugging output, commented-out code or unrelated changes?\n\n")

	b.WriteString("Be proportionate. Block on real problems, not on style preferences you would merely have done differently.\n\n")
	b.WriteString("End your response with exactly one of these lines:\n")
	b.WriteString("VERDICT: APPROVED\n")
	b.WriteString("VERDICT: CHANGES_REQUIRED\n\n")
	b.WriteString("If you require changes, list them above the verdict, specifically enough to act on.\n")
	return b.String()
}

// conflictPrompt asks the agent to resolve a conflicted merge.
func conflictPrompt(fork *domain.Fork, target string, files []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Merging %s into your workstream branch %s produced conflicts.\n\n", target, fork.Branch)
	b.WriteString("Conflicted files:\n")
	for _, file := range files {
		fmt.Fprintf(&b, "- %s\n", file)
	}

	b.WriteString("\nResolve every conflict, then stage the resolutions and complete the merge commit.\n")
	b.WriteString("Both sides are wanted: the other branch is another workstream's finished work, and yours is yours. ")
	b.WriteString("Keep the intent of both. Do not discard the other side's changes to make the conflict go away, ")
	b.WriteString("and do not abandon your own work either.\n")
	b.WriteString("Leave no conflict markers behind. Make sure the project still builds before you finish.\n")
	b.WriteString("\nIf the two sides genuinely cannot both be satisfied, stop and emit:\n")
	b.WriteString("DABBERZ-ESCALATE[ambiguous]: <which two changes conflict and what needs deciding>\n")
	return b.String()
}
