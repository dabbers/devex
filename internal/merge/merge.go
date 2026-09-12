// Package merge lands a finished fork.
//
// Every verified fork goes through a review pass before anything is written to
// a shared branch: a quality gate on the change itself, then conflict
// resolution against the merge target. Both are done by the coding agent
// inside the fork's own VM, which is the only place that has the working tree,
// the toolchain and the context to resolve a conflict sensibly.
//
// Where a fork lands, and when, are per-task user preferences decided
// elsewhere; this package is told the target branch and does the landing.
package merge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// DefaultTimeout bounds a single git invocation.
const DefaultTimeout = 5 * time.Minute

// Config configures the reviewer.
type Config struct {
	// GitBinary is the git executable inside the VM.
	GitBinary string `yaml:"git_binary" json:"git_binary"`
	// Remote is the git remote to fetch from and push to.
	Remote string `yaml:"remote" json:"remote"`
	// Timeout bounds one git command.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// SkipQualityGate disables the review pass. It exists for development;
	// leaving it on is what stops unreviewed work reaching a shared branch.
	SkipQualityGate bool `yaml:"skip_quality_gate" json:"skip_quality_gate"`
	// Push controls whether a successful merge is pushed to the remote.
	Push bool `yaml:"push" json:"push"`
}

func (c Config) withDefaults() Config {
	if c.GitBinary == "" {
		c.GitBinary = "git"
	}
	if c.Remote == "" {
		c.Remote = "origin"
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

// Reviewer lands forks.
type Reviewer struct {
	driver vm.Driver
	agent  *agent.Runner
	cfg    Config
}

// New returns a reviewer that works inside fork VMs.
func New(driver vm.Driver, runner *agent.Runner, cfg Config) *Reviewer {
	return &Reviewer{driver: driver, agent: runner, cfg: cfg.withDefaults()}
}

// Request is one merge attempt.
type Request struct {
	Fork *domain.Fork
	// TargetBranch is where the fork lands: the repo's default branch or the
	// task's integration branch, per the user's preference.
	TargetBranch string
	// InstanceID is the fork's VM.
	InstanceID string
	// Dir is the repo checkout inside the VM. Empty means the workspace.
	Dir string
	// Env carries the repo's inherited secrets and any git credentials.
	Env map[string]string
}

// Outcome says how a merge attempt ended.
type Outcome string

// Merge outcomes.
const (
	// OutcomeMerged means the fork landed on the target branch.
	OutcomeMerged Outcome = "merged"
	// OutcomeRejected means the quality gate refused the change. The fork goes
	// back to the coding agent rather than landing.
	OutcomeRejected Outcome = "rejected"
	// OutcomeNeedsUser means the merge could not be completed automatically.
	OutcomeNeedsUser Outcome = "needs_user"
)

// Result is the outcome of a merge attempt.
type Result struct {
	Outcome Outcome `json:"outcome"`
	// Summary explains the outcome in terms the user can act on.
	Summary string `json:"summary"`
	// ConflictedFiles lists paths that conflicted, whether or not they were
	// resolved.
	ConflictedFiles []string `json:"conflicted_files,omitempty"`
	// ResolvedConflicts reports that the agent resolved conflicts on the way.
	ResolvedConflicts bool `json:"resolved_conflicts"`
	// QualityFeedback is the review agent's verdict, fed back to the coding
	// agent when the gate rejects the change.
	QualityFeedback string `json:"quality_feedback,omitempty"`
	// Usage is what the review and resolution cost.
	Usage domain.Usage `json:"usage"`
}

// Merge runs the quality gate and lands the fork if it passes.
//
// A rejection or an unresolvable conflict is a Result, not an error: both are
// outcomes the lifecycle knows how to route. An error means the merge could
// not be attempted.
func (r *Reviewer) Merge(ctx context.Context, req Request) (*Result, error) {
	if req.InstanceID == "" {
		return nil, fmt.Errorf("merge: fork %s has no VM", req.Fork.ID)
	}
	if req.TargetBranch == "" {
		return nil, fmt.Errorf("merge: fork %s has no target branch", req.Fork.ID)
	}

	result := &Result{}

	// Start from the remote's current view, or the merge is against a stale
	// target and the conflict picture is wrong.
	if out, err := r.git(ctx, req, "fetch", r.cfg.Remote); err != nil {
		return nil, err
	} else if !out.OK() {
		return nil, fmt.Errorf("merge: fetch failed: %s", firstLine(out.Stderr))
	}

	if !r.cfg.SkipQualityGate {
		passed, feedback, usage, err := r.qualityGate(ctx, req)
		if err != nil {
			return nil, err
		}
		result.Usage.Add(usage)
		result.QualityFeedback = feedback
		if !passed {
			// Nothing reaches a shared branch that the gate turned down.
			result.Outcome = OutcomeRejected
			result.Summary = "the review agent found problems that must be fixed before this can land"
			return result, nil
		}
	}

	// Merge the target into the fork first. Resolving on the fork's branch
	// keeps a broken merge off the shared branch entirely: if this goes wrong,
	// the target is untouched.
	mergeOut, err := r.git(ctx, req, "merge", "--no-edit", r.cfg.Remote+"/"+req.TargetBranch)
	if err != nil {
		return nil, err
	}
	if !mergeOut.OK() {
		conflicted, err := r.conflictedFiles(ctx, req)
		if err != nil {
			return nil, err
		}
		result.ConflictedFiles = conflicted
		if len(conflicted) == 0 {
			// A failed merge with no conflicts is something else entirely --
			// a dirty tree, a missing branch -- and guessing would be worse
			// than handing it to the user.
			result.Outcome = OutcomeNeedsUser
			result.Summary = "the merge failed for a reason dabberz could not resolve: " + firstLine(mergeOut.Stderr+mergeOut.Stdout)
			return result, nil
		}

		usage, resolved, err := r.resolveConflicts(ctx, req, conflicted)
		if err != nil {
			return nil, err
		}
		result.Usage.Add(usage)
		if !resolved {
			result.Outcome = OutcomeNeedsUser
			result.Summary = fmt.Sprintf("could not resolve conflicts in %s", strings.Join(conflicted, ", "))
			return result, nil
		}
		result.ResolvedConflicts = true
	}

	if r.cfg.Push {
		// Push the fork branch, then fast-forward the target onto it.
		if out, err := r.git(ctx, req, "push", r.cfg.Remote, req.Fork.Branch); err != nil {
			return nil, err
		} else if !out.OK() {
			result.Outcome = OutcomeNeedsUser
			result.Summary = "could not push the workstream branch: " + firstLine(out.Stderr)
			return result, nil
		}
		refspec := req.Fork.Branch + ":" + req.TargetBranch
		if out, err := r.git(ctx, req, "push", r.cfg.Remote, refspec); err != nil {
			return nil, err
		} else if !out.OK() {
			result.Outcome = OutcomeNeedsUser
			result.Summary = "could not land the change on " + req.TargetBranch + ": " + firstLine(out.Stderr)
			return result, nil
		}
	}

	result.Outcome = OutcomeMerged
	result.Summary = fmt.Sprintf("landed %s on %s", req.Fork.Branch, req.TargetBranch)
	if result.ResolvedConflicts {
		result.Summary += " after resolving conflicts in " + strings.Join(result.ConflictedFiles, ", ")
	}
	return result, nil
}

// qualityGate asks the review agent whether the change is fit to land.
func (r *Reviewer) qualityGate(ctx context.Context, req Request) (bool, string, domain.Usage, error) {
	diff, err := r.git(ctx, req, "diff", r.cfg.Remote+"/"+req.TargetBranch+"..."+req.Fork.Branch)
	if err != nil {
		return false, "", domain.Usage{}, err
	}
	if strings.TrimSpace(diff.Stdout) == "" {
		// Nothing to review and nothing to land.
		return true, "the workstream produced no changes", domain.Usage{}, nil
	}

	res, err := r.agent.Run(ctx, agent.Request{
		InstanceID: req.InstanceID,
		Dir:        req.Dir,
		Env:        req.Env,
		Prompt:     reviewPrompt(req.Fork, req.TargetBranch),
	})
	if err != nil {
		return false, "", domain.Usage{}, fmt.Errorf("merge: quality gate: %w", err)
	}
	if res.Failed {
		// A gate that could not run has not approved anything.
		return false, res.Output, res.Usage, nil
	}
	return verdictApproved(res.Output), res.Output, res.Usage, nil
}

// resolveConflicts hands the conflicted merge to the coding agent.
func (r *Reviewer) resolveConflicts(ctx context.Context, req Request, files []string) (domain.Usage, bool, error) {
	res, err := r.agent.Run(ctx, agent.Request{
		InstanceID: req.InstanceID,
		Dir:        req.Dir,
		Env:        req.Env,
		Prompt:     conflictPrompt(req.Fork, req.TargetBranch, files),
	})
	if err != nil {
		return domain.Usage{}, false, fmt.Errorf("merge: conflict resolution: %w", err)
	}
	if res.Failed || res.Escalation != nil {
		return res.Usage, false, nil
	}

	// Trust the tree, not the agent's account of it: conflicts are only
	// resolved when git says no markers remain.
	remaining, err := r.conflictedFiles(ctx, req)
	if err != nil {
		return res.Usage, false, err
	}
	if len(remaining) > 0 {
		return res.Usage, false, nil
	}

	// An unfinished merge leaves MERGE_HEAD in place even with a clean tree.
	status, err := r.git(ctx, req, "status", "--porcelain")
	if err != nil {
		return res.Usage, false, err
	}
	if strings.Contains(status.Stdout, "UU ") {
		return res.Usage, false, nil
	}
	return res.Usage, true, nil
}

// conflictedFiles lists paths git reports as unmerged.
func (r *Reviewer) conflictedFiles(ctx context.Context, req Request) ([]string, error) {
	out, err := r.git(ctx, req, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		if path := strings.TrimSpace(line); path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

// git runs one git command inside the fork's VM.
func (r *Reviewer) git(ctx context.Context, req Request, args ...string) (*vm.ExecResult, error) {
	out, err := r.driver.Exec(ctx, req.InstanceID, vm.Command{
		Argv:    append([]string{r.cfg.GitBinary}, args...),
		Dir:     req.Dir,
		Env:     req.Env,
		Timeout: r.cfg.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("merge: git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// verdictApproved reads the review agent's verdict. The gate is closed by
// default: anything that is not an explicit approval blocks the merge.
func verdictApproved(output string) bool {
	upper := strings.ToUpper(output)
	if strings.Contains(upper, "VERDICT: CHANGES_REQUIRED") {
		return false
	}
	return strings.Contains(upper, "VERDICT: APPROVED")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return line
	}
	return s
}
