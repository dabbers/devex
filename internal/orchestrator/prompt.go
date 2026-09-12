package orchestrator

import (
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/memory"
)

// planSystemPrompt states the orchestrator's job. It is deliberately explicit
// about two things the rest of the system depends on: that forks are parallel
// workstreams rather than competing attempts at one task, and that overlap is
// judged here, once, because nothing downstream will catch a collision later.
const planSystemPrompt = `You are the orchestrator for dabberz, a platform that runs automated coding agents.

Your job is to turn one user request into a set of independent workstreams, each of which will be
developed by its own coding agent in its own isolated virtual machine, on its own git branch.

Key facts about how these workstreams run:
- They are INDEPENDENT PARALLEL workstreams, not competing attempts at the same task. "Add ratings",
  "add photo upload" and "add rank notes" on one repo are three workstreams, not three tries at one.
- Each runs in a separate VM with no shared state. Agents cannot see or coordinate with each other,
  and there is no mid-flight warning if two of them touch the same code.
- Because of that, you must judge overlap NOW. If two workstreams are likely to collide -- they touch
  the same feature area, the same module, the same data model -- put them in the same serialize_group
  so they run one after another instead of at the same time. Judge this semantically: two workstreams
  that both touch image handling collide even if they sound unrelated.
- Workstreams with no collision risk get an empty serialize_group and run in parallel.

Before any VM is started, you may ask the user clarifying questions. Ask only what actually changes
the plan: an ambiguous scope, a missing decision, a choice between materially different approaches.
Do not ask about things you can reasonably decide yourself. If nothing is genuinely unclear, return
an empty questions array.

Respond with a single JSON object and nothing else:
{
  "summary": "one paragraph describing your reading of the request and how you split it",
  "questions": [
    {"text": "the question", "options": ["optional", "suggested", "answers"]}
  ],
  "workstreams": [
    {
      "name": "short-kebab-case-name",
      "description": "what this workstream should build, in enough detail for an agent to start",
      "project_path": "repo-relative path of the project this targets",
      "serialize_group": "group name shared with workstreams it would collide with, or empty",
      "overlap_rationale": "why you grouped it this way"
    }
  ]
}`

// discoverSystemPrompt drives the monorepo discovery pass. No manifest file is
// required: the layout is inferred and then confirmed with the user once per
// repo.
const discoverSystemPrompt = `You are inspecting a git repository to work out which distinct projects it contains.

A repository may hold one project or several (a monorepo). A project is a separately buildable and
runnable unit: it has its own dependency manifest, its own build, and usually its own dev server.
Directories that merely group source files are not projects.

Respond with a single JSON object and nothing else:
{
  "projects": [
    {
      "name": "short human-readable name",
      "path": "repo-relative path, \".\" for the repository root",
      "toolchain": "go | node | rust | python | ruby | java | other",
      "preview_command": "the command that starts this project's dev server, if it has one",
      "preview_port": 3000
    }
  ]
}`

// planUserPrompt assembles everything the model needs to plan one request.
func planUserPrompt(task *domain.Task, repo *domain.Repo, projects []*domain.Project, findings []*memory.Finding, answered []domain.Question) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Repository: %s (default branch %s)\n\n", repo.Name, repo.DefaultBranch)

	b.WriteString("Projects in this repository:\n")
	if len(projects) == 0 {
		b.WriteString("- (not yet discovered; assume a single project at \".\")\n")
	}
	for _, p := range projects {
		fmt.Fprintf(&b, "- %s at %q", p.Name, p.Path)
		if p.Toolchain != "" {
			fmt.Fprintf(&b, " (%s)", p.Toolchain)
		}
		b.WriteString("\n")
	}

	// Repo memory is what keeps a later plan from re-deriving decisions an
	// earlier one already made.
	if len(findings) > 0 {
		b.WriteString("\nWhat dabberz already knows about this repository:\n")
		for _, f := range findings {
			fmt.Fprintf(&b, "- %s", f.Title)
			if len(f.Tags) > 0 {
				fmt.Fprintf(&b, " [%s]", strings.Join(f.Tags, ", "))
			}
			b.WriteString("\n")
			if body := strings.TrimSpace(f.Body); body != "" {
				fmt.Fprintf(&b, "  %s\n", firstLines(body, 4))
			}
		}
	}

	b.WriteString("\nUser request:\n")
	b.WriteString(task.Request)
	b.WriteString("\n")

	// Answers from an earlier round are carried forward so the model refines
	// its plan rather than asking the same thing again.
	if len(answered) > 0 {
		b.WriteString("\nAnswers you already received to your earlier questions:\n")
		for _, q := range answered {
			fmt.Fprintf(&b, "- Q: %s\n  A: %s\n", q.Text, q.Answer)
		}
		b.WriteString("\nDo not ask these again. Produce a refined plan.\n")
	}

	fmt.Fprintf(&b, "\nMerge preferences for this task: target=%s, timing=%s.\n", task.MergeTarget, task.MergeTiming)
	return b.String()
}

// discoverUserPrompt presents a repo's file listing for the discovery pass.
func discoverUserPrompt(repo *domain.Repo, tree []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\n\nFiles and directories:\n", repo.Name)
	for _, entry := range tree {
		b.WriteString("- ")
		b.WriteString(entry)
		b.WriteString("\n")
	}
	return b.String()
}

// firstLines truncates a body to at most n lines for prompt inclusion, so one
// long finding cannot crowd out the rest.
func firstLines(body string, n int) string {
	lines := strings.Split(body, "\n")
	if len(lines) <= n {
		return strings.Join(lines, " ")
	}
	return strings.Join(lines[:n], " ") + " ..."
}
