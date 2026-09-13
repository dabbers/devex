package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
)

// Overview is everything in flight, across every repo, in one response.
//
// The unit of attention is the fork: that is what holds a machine, spends
// budget and can get stuck. Repos and tasks are shown around it as context,
// so one screen answers "what is running right now, and does anything need
// me" without picking a repo first.
type Overview struct {
	// Repos summarises each repo and what it currently has running.
	Repos []RepoSummary `json:"repos"`
	// Work is every fork that is not finished, newest first, across all repos.
	Work []ForkView `json:"work"`
	// NeedsAttention is the subset waiting on the user. It is separated out
	// because it is the only part of the feed that is actionable.
	NeedsAttention []ForkView `json:"needs_attention"`
	// Totals are the counts a status bar is built from.
	Totals OverviewTotals `json:"totals"`
	// Capacity is the machine and the two queues.
	Capacity map[string]any `json:"capacity"`
	// GeneratedAt is when this snapshot was taken.
	GeneratedAt time.Time `json:"generated_at"`
}

// OverviewTotals counts work by disposition.
type OverviewTotals struct {
	Repos     int `json:"repos"`
	Tasks     int `json:"tasks"`
	Running   int `json:"running"`
	Queued    int `json:"queued"`
	Escalated int `json:"escalated"`
	Merged    int `json:"merged"`
	Failed    int `json:"failed"`
}

// RepoSummary is one repo's slice of the overview.
type RepoSummary struct {
	Repo *domain.Repo `json:"repo"`
	// Tasks are this repo's tasks, newest first, each with its fork counts.
	// They are included here so the dashboard can show a repo and the work
	// under it without a request per repo.
	Tasks []TaskSummary `json:"tasks"`
	// ActiveTasks counts tasks still running in this repo.
	ActiveTasks int `json:"active_tasks"`
	Running     int `json:"running"`
	Queued      int `json:"queued"`
	Escalated   int `json:"escalated"`
	// LastActivity is the most recent event timestamp in this repo, which is
	// what the list is ordered by.
	LastActivity time.Time `json:"last_activity,omitempty"`
}

// TaskSummary is one task and how its forks are doing.
type TaskSummary struct {
	Task *domain.Task `json:"task"`
	// Forks counts the task's workstreams by disposition, which is what the
	// dashboard shows instead of listing them.
	Forks     int `json:"forks"`
	Running   int `json:"running"`
	Queued    int `json:"queued"`
	Escalated int `json:"escalated"`
	Merged    int `json:"merged"`
	// PreviewURL is a live preview belonging to one of this task's forks, so
	// a finished project can be opened straight from the dashboard. Each fork
	// has its own; this is simply the most recently updated one.
	PreviewURL string `json:"preview_url,omitempty"`
}

// ForkView is a fork with the context needed to read it without a second
// request: which repo and task it belongs to, and how much budget is left.
type ForkView struct {
	Fork      *domain.Fork `json:"fork"`
	RepoName  string       `json:"repo_name"`
	TaskTitle string       `json:"task_title"`
	// Remaining is headroom under the global tripwire thresholds.
	Remaining any `json:"budget_remaining,omitempty"`
	// WaitingFor explains a queued fork's wait, so a stalled queue is never
	// unexplained.
	WaitingFor string `json:"waiting_for,omitempty"`
}

// overview builds the cross-repo snapshot the UI opens on.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	repos, err := s.store.ListRepos(ctx, s.owner.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	tasks, err := s.store.ListTasks(ctx, store.TaskFilter{UserID: s.owner.ID})
	if err != nil {
		writeError(w, err)
		return
	}
	forks, err := s.store.ListForks(ctx, store.ForkFilter{UserID: s.owner.ID})
	if err != nil {
		writeError(w, err)
		return
	}

	repoNames := make(map[string]string, len(repos))
	summaries := make(map[string]*RepoSummary, len(repos))
	for _, repo := range repos {
		repoNames[repo.ID] = repo.Name
		summaries[repo.ID] = &RepoSummary{Repo: repo}
	}

	taskTitles := make(map[string]string, len(tasks))
	taskSummaries := make(map[string]*TaskSummary, len(tasks))
	for _, task := range tasks {
		taskTitles[task.ID] = task.Title
		summary := summaries[task.RepoID]
		if summary == nil {
			continue
		}
		if !task.State.Terminal() {
			summary.ActiveTasks++
		}
		summary.Tasks = append(summary.Tasks, TaskSummary{Task: task})
		taskSummaries[task.ID] = &summary.Tasks[len(summary.Tasks)-1]
	}

	// A queued fork's reason comes from the scheduler, so the UI can say what
	// a wait is for instead of showing an unexplained stall.
	waiting := s.waitReasons(ctx)
	thresholds := s.orch.Thresholds()

	// Collections are always present, never null: a client should not have to
	// distinguish "no work" from "field missing".
	overview := Overview{
		Repos:          []RepoSummary{},
		Work:           []ForkView{},
		NeedsAttention: []ForkView{},
		Totals:         OverviewTotals{Repos: len(repos), Tasks: len(tasks)},
		GeneratedAt:    time.Now().UTC(),
	}

	for _, fork := range forks {
		if summary := taskSummaries[fork.TaskID]; summary != nil {
			summary.Forks++
			switch {
			case fork.State == domain.ForkEscalated:
				summary.Escalated++
			case fork.State == domain.ForkQueued:
				summary.Queued++
			case fork.State == domain.ForkMerged:
				summary.Merged++
			case !fork.State.Terminal():
				summary.Running++
			}
			if fork.PreviewURL != "" {
				summary.PreviewURL = fork.PreviewURL
			}
		}

		view := ForkView{
			Fork:      fork,
			RepoName:  repoNames[fork.RepoID],
			TaskTitle: taskTitles[fork.TaskID],
			Remaining: thresholds.Remaining(fork.Usage),
		}
		summary := summaries[fork.RepoID]

		switch {
		case fork.State == domain.ForkEscalated:
			overview.Totals.Escalated++
			if summary != nil {
				summary.Escalated++
			}
			overview.NeedsAttention = append(overview.NeedsAttention, view)
			overview.Work = append(overview.Work, view)
		case fork.State == domain.ForkQueued:
			overview.Totals.Queued++
			if summary != nil {
				summary.Queued++
			}
			view.WaitingFor = waiting[fork.ID]
			overview.Work = append(overview.Work, view)
		case fork.State == domain.ForkMerged:
			overview.Totals.Merged++
		case fork.State == domain.ForkFailed:
			overview.Totals.Failed++
			// A failed fork is finished but still wants a decision from the
			// user, so it belongs with the actionable items.
			overview.NeedsAttention = append(overview.NeedsAttention, view)
		case fork.State.Terminal():
			// Abandoned: finished and needing nothing.
		default:
			overview.Totals.Running++
			if summary != nil {
				summary.Running++
			}
			overview.Work = append(overview.Work, view)
		}
	}

	// Newest work first; a long-running fork should not sit above something
	// that just started and may need attention sooner.
	sort.SliceStable(overview.Work, func(i, j int) bool {
		return overview.Work[i].Fork.CreatedAt.After(overview.Work[j].Fork.CreatedAt)
	})

	if err := s.annotateLastActivity(ctx, summaries); err != nil {
		writeError(w, err)
		return
	}

	overview.Repos = make([]RepoSummary, 0, len(summaries))
	for _, repo := range repos {
		summary := summaries[repo.ID]
		if summary.Tasks == nil {
			summary.Tasks = []TaskSummary{}
		}
		overview.Repos = append(overview.Repos, *summary)
	}
	// Repos with something happening float to the top; ties break on recency.
	sort.SliceStable(overview.Repos, func(i, j int) bool {
		a, b := overview.Repos[i], overview.Repos[j]
		if active := (a.Running + a.Queued + a.Escalated) - (b.Running + b.Queued + b.Escalated); active != 0 {
			return active > 0
		}
		return a.LastActivity.After(b.LastActivity)
	})

	overview.Capacity = s.capacitySnapshot(ctx)
	writeJSON(w, http.StatusOK, overview)
}

// waitReasons maps queued fork ids to why they are still waiting.
func (s *Server) waitReasons(ctx context.Context) map[string]string {
	if s.sched == nil {
		return nil
	}
	// Tick is the scheduler's own admission pass; asking it here would start
	// work as a side effect of rendering a page. Snapshot is read-only.
	snapshot, err := s.sched.Snapshot(ctx)
	if err != nil {
		s.logger.Warn("could not read the scheduler queue", "error", err)
		return nil
	}
	reasons := make(map[string]string, len(snapshot.Queued))
	for _, fork := range snapshot.Queued {
		reasons[fork.ID] = "waiting for capacity or an overlapping sibling"
	}
	return reasons
}

// annotateLastActivity stamps each repo with the time of its newest event.
func (s *Server) annotateLastActivity(ctx context.Context, summaries map[string]*RepoSummary) error {
	for repoID, summary := range summaries {
		events, err := s.store.ListEvents(ctx, store.EventFilter{RepoID: repoID, Limit: 1, Newest: true})
		if err != nil {
			return err
		}
		if len(events) > 0 {
			summary.LastActivity = events[0].CreatedAt
		}
	}
	return nil
}

// capacitySnapshot reports the machine and both queues.
func (s *Server) capacitySnapshot(ctx context.Context) map[string]any {
	snapshot := map[string]any{}
	if s.sched != nil {
		if got, err := s.sched.Snapshot(ctx); err == nil {
			snapshot["machine"] = got.Capacity
			snapshot["queued_forks"] = len(got.Queued)
			snapshot["active_forks"] = len(got.Active)
		}
	}
	if s.verifier != nil {
		snapshot["verification"] = s.verifier.Stats()
	}
	return snapshot
}

// audit serves the unified cross-repo audit trail.
//
// This is the same event log the per-task and per-fork views read; the
// difference is only which filters are applied, so there is one trail rather
// than a separate summary that could drift from it.
func (s *Server) auditFeed(w http.ResponseWriter, r *http.Request) {
	filter, err := s.auditFilter(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.Newest = true

	events, err := s.store.ListEvents(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	if events == nil {
		events = []*domain.Event{}
	}

	// Newest-first is right for reading, but a cursor has to be the highest
	// sequence seen, so it is reported separately rather than inferred from
	// the order of the rows.
	var cursor int64
	for _, event := range events {
		if event.Seq > cursor {
			cursor = event.Seq
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"cursor": cursor,
		"actors": knownActors(),
	})
}

// auditFilter reads the filter parameters shared by the feed and its stream.
func (s *Server) auditFilter(r *http.Request) (store.EventFilter, error) {
	query := r.URL.Query()
	filter := store.EventFilter{
		UserID:    s.owner.ID,
		RepoID:    query.Get("repo"),
		TaskID:    query.Get("task"),
		ForkID:    query.Get("fork"),
		Actor:     domain.Actor(query.Get("actor")),
		AfterSeq:  int64(intParam(r, "after", 0)),
		BeforeSeq: int64(intParam(r, "before", 0)),
		Limit:     intParam(r, "limit", 200),
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	if types := strings.TrimSpace(query.Get("type")); types != "" {
		for _, name := range strings.Split(types, ",") {
			if name = strings.TrimSpace(name); name != "" {
				filter.Types = append(filter.Types, domain.EventType(name))
			}
		}
	}
	return filter, nil
}

// knownActors lists the actors the UI offers as filters.
func knownActors() []domain.Actor {
	return []domain.Actor{
		domain.ActorUser,
		domain.ActorOrchestrator,
		domain.ActorScheduler,
		domain.ActorPipeline,
		domain.ActorAgent,
		domain.ActorVerifier,
		domain.ActorReviewer,
	}
}

// validateFork re-runs browser validation against a sub-task's live preview on
// request.
//
// This deliberately does not touch the fork's state. The pipeline owns the
// verify/fix loop, and a manual run that moved the fork between states would
// race with it. What this gives the user is a fresh answer to "does the
// preview still work", recorded on the trail like any other validation; the
// verifier's own queue keeps it from oversubscribing the shared UI VM.
func (s *Server) validateFork(w http.ResponseWriter, r *http.Request) {
	if s.verifier == nil {
		writeStatus(w, http.StatusServiceUnavailable, "no shared UI VM is configured, so nothing can be validated")
		return
	}

	ctx := r.Context()
	fork, err := s.store.GetFork(ctx, r.PathValue("fork"))
	if err != nil {
		writeError(w, err)
		return
	}
	if fork.PreviewURL == "" {
		writeStatus(w, http.StatusConflict, "this sub-task has no live preview yet")
		return
	}

	// The request is the user's; the finding is the verifier's. Recording them
	// separately is what makes the trail answer who asked and what was found.
	s.audit(ctx, &domain.Event{
		RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Type:    domain.EventVerifyStarted,
		Message: "validation requested by hand",
		Data:    map[string]any{"manual": true, "preview_url": fork.PreviewURL},
	})

	report, err := s.verifier.Verify(ctx, verify.Request{
		ForkID:     fork.ID,
		PreviewURL: fork.PreviewURL,
		Intent:     fork.Description,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	if err := s.store.AppendEvent(ctx, &domain.Event{
		UserID: fork.UserID, RepoID: fork.RepoID, TaskID: fork.TaskID, ForkID: fork.ID,
		Actor: domain.ActorVerifier, Type: domain.EventVerifyFinished,
		Message: report.Summary,
		Data: map[string]any{
			"manual": true, "passed": report.Passed,
			"duration_seconds": report.Duration.Seconds(),
		},
	}); err != nil {
		s.logger.Error("could not record a manual validation", "fork", fork.ID, "error", err)
	}

	writeJSON(w, http.StatusOK, report)
}
