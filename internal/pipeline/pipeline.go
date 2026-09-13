// Package pipeline drives one fork from admission to merge.
//
// It is what the scheduler hands an admitted fork to, and it owns the
// verify/fix loop: provision a VM, publish a preview, run the coding agent,
// verify against the live preview, feed failures back, and hand the finished
// work to the merge gate. The loop is fully automatic. It reaches the user
// only in the three cases the requirements allow: the fix conflicts with the
// instructions, the situation is genuinely ambiguous, or a tripwire says the
// agent is stuck beyond its own ability to resolve.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/merge"
	"github.com/dabbers/devex/internal/orchestrator"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
)

// Options configures the pipeline.
type Options struct {
	// ForkResources is the VM allocation each fork receives.
	ForkResources vm.Resources
	// Image is the golden image every fork boots.
	Image string
	// MemoryMount is where a repo's notes are mounted inside the fork VM.
	MemoryMount string
	Logger      *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.ForkResources == (vm.Resources{}) {
		o.ForkResources = vm.Resources{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 20}
	}
	if o.Image == "" {
		o.Image = "dabberz-golden"
	}
	if o.MemoryMount == "" {
		o.MemoryMount = "/srv/dabberz/memory"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Pipeline runs forks end to end.
type Pipeline struct {
	store    *store.Store
	driver   vm.Driver
	agent    *agent.Runner
	verifier *verify.Verifier
	reviewer *merge.Reviewer
	preview  *preview.Allocator
	vault    *secrets.Vault
	orch     *orchestrator.Orchestrator
	opts     Options
}

// Deps are the collaborators a pipeline needs.
type Deps struct {
	Store    *store.Store
	Driver   vm.Driver
	Agent    *agent.Runner
	Verifier *verify.Verifier
	Reviewer *merge.Reviewer
	Preview  *preview.Allocator
	Vault    *secrets.Vault
	Orch     *orchestrator.Orchestrator
}

// New returns a pipeline.
func New(deps Deps, opts Options) (*Pipeline, error) {
	if deps.Store == nil || deps.Driver == nil || deps.Agent == nil || deps.Orch == nil {
		return nil, errors.New("pipeline: store, driver, agent and orchestrator are required")
	}
	// The verifier is not optional. Every finished fork is checked by driving
	// a real browser against its live preview, and there is no path that lets
	// unverified work reach the merge gate -- the fork state machine has no
	// edge from coding to awaiting_merge, so a missing verifier would strand
	// every fork rather than quietly merging it.
	if deps.Verifier == nil {
		return nil, errors.New("pipeline: a verifier is required; nothing merges unverified")
	}
	return &Pipeline{
		store: deps.Store, driver: deps.Driver, agent: deps.Agent,
		verifier: deps.Verifier, reviewer: deps.Reviewer, preview: deps.Preview,
		vault: deps.Vault, orch: deps.Orch, opts: opts.withDefaults(),
	}, nil
}

// Launch implements scheduler.Launcher. It runs a fork to completion and never
// returns an error: everything is recorded against the fork instead, because
// nothing is watching this goroutine.
func (p *Pipeline) Launch(ctx context.Context, fork *domain.Fork) {
	if err := p.run(ctx, fork); err != nil {
		p.opts.Logger.Error("fork pipeline failed", "fork", fork.ID, "error", err)
		p.failFork(ctx, fork, err)
	}
	// A finished fork may complete its task, and may free a slot or a
	// serialization group for whatever is queued behind it.
	if _, err := p.orch.Reconcile(ctx, fork.TaskID); err != nil {
		p.opts.Logger.Error("could not reconcile task", "task", fork.TaskID, "error", err)
	}
}

// run drives the fork's lifecycle.
func (p *Pipeline) run(ctx context.Context, fork *domain.Fork) error {
	repo, err := p.store.GetRepo(ctx, fork.RepoID)
	if err != nil {
		return err
	}
	project, err := p.store.GetProject(ctx, fork.ProjectID)
	if err != nil {
		return err
	}

	env, err := p.environment(ctx, fork.RepoID)
	if err != nil {
		return err
	}

	instance, previewURL, err := p.provision(ctx, fork, repo, project, env)
	if err != nil {
		return err
	}

	// First coding pass.
	if err := p.transition(ctx, fork, domain.ForkCoding, "coding agent started"); err != nil {
		return err
	}
	prompt := agent.BuildPrompt(fork, repo, project, previewURL, p.opts.MemoryMount)
	stopped, err := p.code(ctx, fork, instance.ID, prompt, env)
	if err != nil || stopped {
		return err
	}

	// Verify, then merge. A merge review that requests changes puts the fork
	// back into fixing, and the whole verify-then-merge sequence runs again:
	// reworked code is never offered to the gate unverified. The tripwire's
	// cycle counter is what bounds this loop.
	for {
		if err := p.verifyLoop(ctx, fork, instance.ID, previewURL, env); err != nil {
			return err
		}
		if fork.State != domain.ForkAwaitingMerge {
			// The loop ended in an escalation or a failure; nothing to merge.
			return nil
		}
		if err := p.landIfReady(ctx, fork, instance.ID, env); err != nil {
			return err
		}
		if fork.State != domain.ForkFixing {
			return nil
		}
	}
}

// provision boots the fork's VM and publishes its preview.
func (p *Pipeline) provision(ctx context.Context, fork *domain.Fork, repo *domain.Repo, project *domain.Project, env map[string]string) (*vm.Instance, string, error) {
	instance, err := p.driver.Create(ctx, vm.Spec{
		ForkID:    fork.ID,
		Name:      fork.Name,
		Resources: p.opts.ForkResources,
		Image:     p.opts.Image,
		Env:       env,
		Labels: map[string]string{
			"dabberz.fork": fork.ID,
			"dabberz.task": fork.TaskID,
			"dabberz.repo": repo.Name,
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("pipeline: provision VM for %s: %w", fork.ID, err)
	}

	fork.InstanceID = instance.ID
	if err := p.store.UpdateFork(ctx, fork); err != nil {
		return nil, "", err
	}
	// Record which credentials the fork was handed. Names only: the audit
	// trail is stored in the clear, and "which secrets did this agent get" is
	// the question worth answering anyway.
	p.event(ctx, fork, domain.EventVMProvisioned, "VM provisioned", map[string]any{
		"instance":         instance.ID,
		"address":          instance.Address,
		"secrets_injected": sortedKeys(env),
		"resources":        p.opts.ForkResources.String(),
		"image":            p.opts.Image,
	})

	previewURL := ""
	if p.preview != nil {
		route, err := p.preview.Allocate(ctx, fork, project, instance.Address)
		if err != nil {
			return nil, "", err
		}
		if err := p.preview.Publish(ctx); err != nil {
			return nil, "", err
		}
		previewURL = p.preview.URL(route)
		fork.PreviewURL = previewURL
		if err := p.store.UpdateFork(ctx, fork); err != nil {
			return nil, "", err
		}
		p.event(ctx, fork, domain.EventPreviewReady, "preview available at "+previewURL, map[string]any{
			"url": previewURL, "port": route.UpstreamPort,
		})
	}
	return instance, previewURL, nil
}

// code runs the coding agent once and routes the outcome. It reports whether
// the fork has left the working states, which happens when the agent escalated
// or the run failed outright.
func (p *Pipeline) code(ctx context.Context, fork *domain.Fork, instanceID, prompt string, env map[string]string) (bool, error) {
	res, err := p.agent.Run(ctx, agent.Request{
		InstanceID: instanceID,
		Prompt:     prompt,
		Env:        env,
	})
	if err != nil {
		return true, err
	}

	fork.Usage.Add(res.Usage)
	if err := p.store.UpdateFork(ctx, fork); err != nil {
		return true, err
	}
	p.eventAs(ctx, fork, domain.ActorAgent, domain.EventAgentMessage, truncate(res.Output, 400), map[string]any{
		"cost_usd":       res.Usage.CostUSD,
		"tokens":         res.Usage.InputTokens + res.Usage.OutputTokens,
		"usage_reported": res.UsageReported,
	})
	if !res.UsageReported {
		// Zero usage here means unaccounted, not free. Say so: the cost and
		// token tripwires are not counting this run, and a budget that
		// silently stops accruing is worse than one that is visibly wrong.
		p.eventAs(ctx, fork, domain.ActorPipeline, domain.EventError,
			"the coding agent did not report what this run cost; cost and token budgets are not counting it", nil)
	}

	// A rate limit is not a failure of the work; it is a pause, surfaced to
	// the user rather than pre-empted by an artificial concurrency cap.
	if res.RateLimited {
		return true, p.orch.Escalate(ctx, fork, domain.EscalationRateLimited,
			"the coding agent hit an upstream rate limit", res.Output)
	}
	if res.Escalation != nil {
		return true, p.orch.Escalate(ctx, fork, res.Escalation.Kind, res.Escalation.Message, res.Output)
	}
	if res.Failed {
		return true, p.transition(ctx, fork, domain.ForkFailed, truncate(res.Output, 400))
	}

	// Spend accrues across the whole loop, so check it after every pass.
	fired, err := p.orch.CheckTripwire(ctx, fork)
	return fired, err
}

// verifyLoop runs verification and feeds failures back until the fork passes,
// escalates or trips a wire.
func (p *Pipeline) verifyLoop(ctx context.Context, fork *domain.Fork, instanceID, previewURL string, env map[string]string) error {
	for {
		if err := p.transition(ctx, fork, domain.ForkVerifying, "verifying against the live preview"); err != nil {
			return err
		}
		p.eventAs(ctx, fork, domain.ActorVerifier, domain.EventVerifyStarted, "verification started", nil)

		report, err := p.verifier.Verify(ctx, verify.Request{
			ForkID:     fork.ID,
			PreviewURL: previewURL,
			Intent:     fork.Description,
		})
		if err != nil {
			return err
		}
		p.eventAs(ctx, fork, domain.ActorVerifier, domain.EventVerifyFinished, report.Summary, map[string]any{
			"passed": report.Passed, "duration_seconds": report.Duration.Seconds(),
		})

		if report.Passed {
			return p.transition(ctx, fork, domain.ForkAwaitingMerge, "verification passed")
		}

		// A failed round is a completed cycle, and the cycle count is what
		// tells the tripwire the loop is not converging.
		fork.Usage.Cycles++
		if err := p.store.UpdateFork(ctx, fork); err != nil {
			return err
		}
		fired, err := p.orch.CheckTripwire(ctx, fork)
		if err != nil {
			return err
		}
		if fired {
			return nil
		}

		if err := p.transition(ctx, fork, domain.ForkFixing, "feeding the verification report back"); err != nil {
			return err
		}
		stopped, err := p.code(ctx, fork, instanceID, agent.BuildFixPrompt(fork, report.FeedbackText(), fork.Usage.Cycles), env)
		if err != nil || stopped {
			return err
		}
	}
}

// landIfReady merges the fork when its merge window has arrived.
func (p *Pipeline) landIfReady(ctx context.Context, fork *domain.Fork, instanceID string, env map[string]string) error {
	if p.reviewer == nil {
		return nil
	}

	ready, err := p.orch.ReadyToMerge(ctx, fork.TaskID)
	if err != nil {
		return err
	}
	// Under batch timing this fork waits in awaiting_merge until its siblings
	// finish; whichever fork finishes last lands the whole batch.
	for _, candidate := range ready {
		if candidate.ID == fork.ID {
			return p.land(ctx, fork, instanceID, env)
		}
		if err := p.land(ctx, candidate, candidate.InstanceID, env); err != nil {
			p.opts.Logger.Error("could not land sibling fork", "fork", candidate.ID, "error", err)
		}
	}
	return nil
}

// land runs the merge gate for one fork.
func (p *Pipeline) land(ctx context.Context, fork *domain.Fork, instanceID string, env map[string]string) error {
	task, err := p.store.GetTask(ctx, fork.TaskID)
	if err != nil {
		return err
	}
	repo, err := p.store.GetRepo(ctx, fork.RepoID)
	if err != nil {
		return err
	}

	target := repo.DefaultBranch
	if task.MergeTarget == domain.MergeTargetIntegrationBranch && task.IntegrationBranch != "" {
		target = task.IntegrationBranch
	}

	// Claim the fork before merging. Under batch timing several pipeline
	// goroutines look at the same finished forks at once, and without this
	// two of them land the same one -- merging and pushing it twice.
	claimed, err := p.store.ClaimFork(ctx, fork, domain.ForkAwaitingMerge, domain.ForkMerging, "merging into "+target)
	if err != nil {
		return err
	}
	if !claimed {
		// Another goroutine is landing it, or already has.
		return nil
	}
	p.event(ctx, fork, domain.EventForkStateChange, "awaiting_merge -> merging", map[string]any{
		"from": string(domain.ForkAwaitingMerge), "to": string(domain.ForkMerging), "reason": "merging into " + target,
	})

	res, err := p.reviewer.Merge(ctx, merge.Request{
		Fork: fork, TargetBranch: target, InstanceID: instanceID, Env: env,
	})
	if err != nil {
		return err
	}
	fork.Usage.Add(res.Usage)
	if err := p.store.UpdateFork(ctx, fork); err != nil {
		return err
	}

	switch res.Outcome {
	case merge.OutcomeMerged:
		if err := p.transition(ctx, fork, domain.ForkMerged, res.Summary); err != nil {
			return err
		}
		p.eventAs(ctx, fork, domain.ActorReviewer, domain.EventForkMerged, res.Summary, map[string]any{"target": target})
		return nil

	case merge.OutcomeRejected:
		// The gate found real problems, so the work goes back to the agent
		// rather than landing or stopping.
		return p.reworkAfterReview(ctx, fork, instanceID, env, res.QualityFeedback)

	default:
		return p.orch.Escalate(ctx, fork, domain.EscalationAmbiguous, res.Summary, res.QualityFeedback)
	}
}

// reworkAfterReview sends a rejected change back to the coding agent. The fork
// is left in fixing so the caller re-enters the verify loop: a reworked change
// is verified again before the gate sees it a second time.
func (p *Pipeline) reworkAfterReview(ctx context.Context, fork *domain.Fork, instanceID string, env map[string]string, feedback string) error {
	// A rejected review is a completed round, and counting it is what stops
	// an agent and a reviewer disagreeing forever.
	fork.Usage.Cycles++
	if err := p.store.UpdateFork(ctx, fork); err != nil {
		return err
	}
	fired, err := p.orch.CheckTripwire(ctx, fork)
	if err != nil || fired {
		return err
	}

	if err := p.transition(ctx, fork, domain.ForkFixing, "merge review requested changes"); err != nil {
		return err
	}
	_, err = p.code(ctx, fork, instanceID, agent.BuildFixPrompt(fork, feedback, fork.Usage.Cycles), env)
	return err
}

// environment assembles the variables a fork's VM receives: the repo's secrets,
// inherited wholesale, since the repo is the root of the hierarchy.
func (p *Pipeline) environment(ctx context.Context, repoID string) (map[string]string, error) {
	if p.vault == nil {
		return map[string]string{}, nil
	}
	env, err := p.vault.Environment(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: resolve secrets for repo %s: %w", repoID, err)
	}
	return env, nil
}

// transition moves the fork and records it.
func (p *Pipeline) transition(ctx context.Context, fork *domain.Fork, next domain.ForkState, reason string) error {
	from := fork.State
	if err := p.store.TransitionFork(ctx, fork, next, reason); err != nil {
		return err
	}
	p.event(ctx, fork, domain.EventForkStateChange, fmt.Sprintf("%s -> %s", from, next), map[string]any{
		"from": string(from), "to": string(next), "reason": reason,
	})
	return nil
}

// failFork records an unrecoverable pipeline error against the fork.
func (p *Pipeline) failFork(ctx context.Context, fork *domain.Fork, cause error) {
	p.event(ctx, fork, domain.EventError, cause.Error(), nil)
	if fork.State.Terminal() || fork.State == domain.ForkEscalated {
		return
	}
	if err := p.store.TransitionFork(ctx, fork, domain.ForkFailed, truncate(cause.Error(), 400)); err != nil {
		// The fork may be in a state that cannot reach failed; record it and
		// move on rather than looping.
		p.opts.Logger.Error("could not mark fork failed", "fork", fork.ID, "state", fork.State, "error", err)
	}
}

// event records an activity-feed entry attributed to the pipeline itself.
func (p *Pipeline) event(ctx context.Context, fork *domain.Fork, kind domain.EventType, message string, data map[string]any) {
	p.eventAs(ctx, fork, domain.ActorPipeline, kind, message, data)
}

// eventAs records an entry attributed to a particular actor, so the audit
// trail distinguishes what the agent said from what the verifier found.
func (p *Pipeline) eventAs(ctx context.Context, fork *domain.Fork, actor domain.Actor, kind domain.EventType, message string, data map[string]any) {
	err := p.store.AppendEvent(ctx, &domain.Event{
		UserID: fork.UserID, RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Actor: actor, Type: kind, Message: message, Data: data,
	})
	if err != nil {
		p.opts.Logger.Error("could not record event", "fork", fork.ID, "type", kind, "error", err)
	}
}

// sortedKeys returns a map's keys in a stable order, for audit entries that
// must read the same way every time.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// truncate clips text for storage in a feed entry.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
