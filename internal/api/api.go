// Package api serves the dabberz web control plane.
//
// v1 is driven from the web only; Discord and Telegram are later interfaces
// onto the same orchestrator, which is why the orchestration logic lives
// behind this package rather than inside it. Everything here is a thin
// translation between HTTP and the orchestrator, plus the live activity
// stream the workspace view is built on.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/orchestrator"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/scheduler"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
)

// Server serves the control-plane HTTP API.
type Server struct {
	store     *store.Store
	orch      *orchestrator.Orchestrator
	sched     *scheduler.Scheduler
	preview   *preview.Allocator
	vault     *secrets.Vault
	memory    *memory.Store
	verifier  *verify.Verifier
	driver    vm.Driver
	ui        http.Handler
	owner     *domain.User
	logger    *slog.Logger
	streamGap time.Duration
}

// Deps are the collaborators the API exposes.
type Deps struct {
	Store    *store.Store
	Orch     *orchestrator.Orchestrator
	Sched    *scheduler.Scheduler
	Preview  *preview.Allocator
	Vault    *secrets.Vault
	Memory   *memory.Store
	Verifier *verify.Verifier
	// Driver is the VM driver, used to open a shell on a sub-task's machine.
	// It is optional; without it the workspace shell is unavailable.
	Driver vm.Driver
	// UI serves the control-plane web interface. It is optional so the API can
	// run headless.
	UI http.Handler
	// Owner is the single v1 user every request is scoped to.
	Owner  *domain.User
	Logger *slog.Logger
}

// New returns an API server.
func New(deps Deps) (*Server, error) {
	if deps.Store == nil || deps.Orch == nil || deps.Owner == nil {
		return nil, errors.New("api: store, orchestrator and owner are required")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store: deps.Store, orch: deps.Orch, sched: deps.Sched,
		preview: deps.Preview, vault: deps.Vault, memory: deps.Memory,
		verifier: deps.Verifier, driver: deps.Driver, ui: deps.UI,
		owner: deps.Owner, logger: logger,
		// How often the event stream polls for new entries. The store is local,
		// so this is cheap.
		streamGap: time.Second,
	}, nil
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/capacity", s.capacity)
	// The two surfaces the UI opens on: what is in flight everywhere, and the
	// unified audit trail behind it.
	mux.HandleFunc("GET /v1/overview", s.overview)
	mux.HandleFunc("GET /v1/audit", s.auditFeed)

	mux.HandleFunc("GET /v1/repos", s.listRepos)
	mux.HandleFunc("POST /v1/repos", s.createRepo)
	mux.HandleFunc("GET /v1/repos/{repo}", s.getRepo)
	mux.HandleFunc("GET /v1/repos/{repo}/projects", s.listProjects)
	mux.HandleFunc("POST /v1/repos/{repo}/discover", s.discoverProjects)
	mux.HandleFunc("POST /v1/repos/{repo}/projects/confirm", s.confirmProjects)

	mux.HandleFunc("GET /v1/repos/{repo}/secrets", s.listSecrets)
	mux.HandleFunc("PUT /v1/repos/{repo}/secrets/{name}", s.putSecret)
	mux.HandleFunc("DELETE /v1/repos/{repo}/secrets/{name}", s.deleteSecret)

	mux.HandleFunc("GET /v1/repos/{repo}/memory", s.listFindings)
	mux.HandleFunc("PUT /v1/repos/{repo}/memory/{slug}", s.putFinding)

	mux.HandleFunc("GET /v1/tasks", s.listTasks)
	mux.HandleFunc("POST /v1/tasks", s.createTask)
	mux.HandleFunc("GET /v1/tasks/{task}", s.getTask)
	mux.HandleFunc("POST /v1/tasks/{task}/plan", s.planTask)
	mux.HandleFunc("POST /v1/tasks/{task}/answers", s.answerQuestions)
	mux.HandleFunc("POST /v1/tasks/{task}/approve", s.approvePlan)
	mux.HandleFunc("POST /v1/tasks/{task}/cancel", s.cancelTask)
	mux.HandleFunc("GET /v1/tasks/{task}/forks", s.listForks)

	mux.HandleFunc("GET /v1/forks/{fork}", s.getFork)
	mux.HandleFunc("POST /v1/forks/{fork}/resolve", s.resolveEscalation)
	mux.HandleFunc("POST /v1/forks/{fork}/validate", s.validateFork)
	mux.HandleFunc("GET /v1/forks/{fork}/shell", s.shell)

	mux.HandleFunc("GET /v1/events", s.listEvents)
	mux.HandleFunc("GET /v1/events/stream", s.streamEvents)

	// The UI is served from the same origin as the API, so the browser needs
	// no cross-origin configuration and the page can stream events directly.
	// More specific patterns above still win, so this only catches UI routes.
	if s.ui != nil {
		mux.Handle("GET /", s.ui)
	}

	return logRequests(s.logger, mux)
}

// audit records a user action. Actions taken through the control plane are
// attributed to the user; components attribute their own work themselves.
// A failure to record is logged rather than failing the request: losing an
// audit entry is bad, but refusing the action the user already took is worse
// and would leave the system and its trail disagreeing.
func (s *Server) audit(ctx context.Context, e *domain.Event) {
	e.UserID = s.owner.ID
	e.Actor = domain.ActorUser
	if err := s.store.AppendEvent(ctx, e); err != nil {
		s.logger.Error("could not record an audit entry", "type", e.Type, "error", err)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// capacity reports what the machine is doing, including the queues. Both
// queues behave the same way -- work waits rather than displacing anything --
// so the UI can present them together.
func (s *Server) capacity(w http.ResponseWriter, r *http.Request) {
	response := map[string]any{}

	if s.sched != nil {
		snapshot, err := s.sched.Snapshot(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		response["machine"] = snapshot.Capacity
		response["queued_forks"] = len(snapshot.Queued)
		response["active_forks"] = len(snapshot.Active)
	}
	if s.verifier != nil {
		response["verification"] = s.verifier.Stats()
	}
	writeJSON(w, http.StatusOK, response)
}

// createRepoRequest registers a repo with dabberz.
type createRepoRequest struct {
	Name          string `json:"name"`
	RemoteURL     string `json:"remote_url"`
	DefaultBranch string `json:"default_branch"`
}

func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	var req createRepoRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.RemoteURL == "" {
		writeStatus(w, http.StatusBadRequest, "name and remote_url are required")
		return
	}

	repo := &domain.Repo{
		UserID: s.owner.ID, Name: req.Name,
		RemoteURL: req.RemoteURL, DefaultBranch: req.DefaultBranch,
	}
	if err := s.store.CreateRepo(r.Context(), repo); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r.Context(), &domain.Event{
		RepoID: repo.ID, Type: domain.EventRepoCreated,
		Message: "repo added: " + repo.Name,
		Data:    map[string]any{"remote_url": repo.RemoteURL, "default_branch": repo.DefaultBranch},
	})
	writeJSON(w, http.StatusCreated, repo)
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepos(r.Context(), s.owner.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.store.GetRepo(r.Context(), r.PathValue("repo"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context(), r.PathValue("repo"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// discoverRequest carries the repo listing the discovery pass reads.
type discoverRequest struct {
	Tree []string `json:"tree"`
}

func (s *Server) discoverProjects(w http.ResponseWriter, r *http.Request) {
	var req discoverRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projects, err := s.orch.DiscoverProjects(r.Context(), r.PathValue("repo"), req.Tree)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// confirmRequest is the user accepting a discovered layout.
type confirmRequest struct {
	Paths []string `json:"paths"`
}

func (s *Server) confirmProjects(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	projects, err := s.orch.ConfirmProjects(r.Context(), r.PathValue("repo"), req.Paths)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	// Names only. Listing is a UI operation and has no business decrypting.
	names, err := s.vault.Names(r.Context(), r.PathValue("repo"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

// putSecretRequest sets one repo-scoped secret.
type putSecretRequest struct {
	Value string `json:"value"`
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	var req putSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	repoID, name := r.PathValue("repo"), r.PathValue("name")
	if err := s.vault.Set(r.Context(), repoID, name, req.Value); err != nil {
		writeError(w, err)
		return
	}
	// The name is recorded, never the value: this trail is stored in the clear
	// and is meant to be read.
	s.audit(r.Context(), &domain.Event{
		RepoID: repoID, Type: domain.EventSecretSet,
		Message: "secret set: " + name,
		Data:    map[string]any{"name": name},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	repoID, name := r.PathValue("repo"), r.PathValue("name")
	if err := s.vault.Delete(r.Context(), repoID, name); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r.Context(), &domain.Event{
		RepoID: repoID, Type: domain.EventSecretDeleted,
		Message: "secret deleted: " + name,
		Data:    map[string]any{"name": name},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listFindings(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		writeStatus(w, http.StatusServiceUnavailable, "the memory store is not configured")
		return
	}
	query := r.URL.Query().Get("q")
	var tags []string
	if tag := r.URL.Query().Get("tag"); tag != "" {
		tags = append(tags, tag)
	}
	findings, err := s.memory.Search(r.Context(), r.PathValue("repo"), query, tags...)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": findings})
}

func (s *Server) putFinding(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		writeStatus(w, http.StatusServiceUnavailable, "the memory store is not configured")
		return
	}
	var finding memory.Finding
	if !decodeJSON(w, r, &finding) {
		return
	}
	finding.Slug = r.PathValue("slug")
	if err := s.memory.Put(r.Context(), r.PathValue("repo"), &finding); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &finding)
}

// createTaskRequest is a new user request.
type createTaskRequest struct {
	RepoID  string `json:"repo_id"`
	Title   string `json:"title"`
	Request string `json:"request"`
	// MergeTarget and MergeTiming are per-task preferences. They are sent
	// explicitly rather than inferred from anything.
	MergeTarget       domain.MergeTarget `json:"merge_target"`
	MergeTiming       domain.MergeTiming `json:"merge_timing"`
	IntegrationBranch string             `json:"integration_branch"`
	// Plan asks the orchestrator to plan immediately, which is the usual flow.
	Plan bool `json:"plan"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	task, err := s.orch.CreateTask(r.Context(), orchestrator.CreateTaskRequest{
		UserID: s.owner.ID, RepoID: req.RepoID, Title: req.Title, Request: req.Request,
		MergeTarget: req.MergeTarget, MergeTiming: req.MergeTiming,
		IntegrationBranch: req.IntegrationBranch,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Plan {
		if task, err = s.orch.Plan(r.Context(), task.ID); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(r.Context(), store.TaskFilter{
		UserID: s.owner.ID,
		RepoID: r.URL.Query().Get("repo"),
		Limit:  intParam(r, "limit", 50),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.GetTask(r.Context(), r.PathValue("task"))
	if err != nil {
		writeError(w, err)
		return
	}
	forks, err := s.store.ListForks(r.Context(), store.ForkFilter{TaskID: task.ID})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "forks": forks})
}

func (s *Server) planTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.orch.Plan(r.Context(), r.PathValue("task"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// answersRequest maps question ids to the user's answers.
type answersRequest struct {
	Answers map[string]string `json:"answers"`
}

func (s *Server) answerQuestions(w http.ResponseWriter, r *http.Request) {
	var req answersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	task, err := s.orch.AnswerQuestions(r.Context(), r.PathValue("task"), req.Answers)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	task, forks, err := s.orch.ApprovePlan(r.Context(), r.PathValue("task"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "forks": forks})
}

// cancelRequest carries an optional reason.
type cancelRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	// A cancellation with no body is fine.
	_ = json.NewDecoder(r.Body).Decode(&req)

	if err := s.orch.Cancel(r.Context(), r.PathValue("task"), req.Reason); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listForks(w http.ResponseWriter, r *http.Request) {
	forks, err := s.store.ListForks(r.Context(), store.ForkFilter{TaskID: r.PathValue("task")})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forks": forks})
}

func (s *Server) getFork(w http.ResponseWriter, r *http.Request) {
	fork, err := s.store.GetFork(r.Context(), r.PathValue("fork"))
	if err != nil {
		writeError(w, err)
		return
	}

	response := map[string]any{
		"fork": fork,
		// Headroom under the global thresholds, so the UI can show how much
		// budget a running fork has left.
		"budget_remaining": s.orch.Thresholds().Remaining(fork.Usage),
	}
	if s.preview != nil {
		if route, err := s.store.GetPreviewRoute(r.Context(), fork.ID); err == nil {
			response["preview"] = route
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// resolveRequest is the user answering an escalation.
type resolveRequest struct {
	Response string `json:"response"`
	// Resume is the state to return the fork to; it defaults to coding.
	Resume domain.ForkState `json:"resume"`
}

func (s *Server) resolveEscalation(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fork, err := s.orch.ResolveEscalation(r.Context(), r.PathValue("fork"), req.Response, req.Resume)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fork)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	filter, err := s.auditFilter(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	events, err := s.store.ListEvents(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// streamEvents serves the activity feed as server-sent events.
//
// The cursor is the event sequence number, so a client that drops the
// connection resumes exactly where it left off without replaying or skipping.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeStatus(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	filter, err := s.auditFilter(r)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
		return
	}
	// A stream always reads forwards from its cursor, whatever order the
	// paged feed uses.
	filter.Newest = false

	ticker := time.NewTicker(s.streamGap)
	defer ticker.Stop()

	for {
		events, err := s.store.ListEvents(r.Context(), filter)
		if err != nil {
			// The client is already streaming, so the only thing left is to
			// tell it and stop.
			fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
			flusher.Flush()
			return
		}
		for _, event := range events {
			body, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Seq, body)
			filter.AfterSeq = event.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// intParam reads an integer query parameter, falling back to a default.
func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

// decodeJSON reads a JSON body, writing a 400 and reporting false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	// Reject unknown fields: a typo in a merge preference should be an error,
	// not a silently ignored key that changes where work lands.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeStatus(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // the client has gone; there is nothing to do about it
	json.NewEncoder(w).Encode(body)
}

func writeStatus(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeError maps domain errors onto status codes.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, secrets.ErrNotFound), errors.Is(err, memory.ErrNotFound):
		writeStatus(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeStatus(w, http.StatusConflict, err.Error())
	case isStateError(err):
		// Asking for something the lifecycle does not allow is a client
		// mistake, not a server failure.
		writeStatus(w, http.StatusConflict, err.Error())
	case errors.Is(err, context.Canceled):
		writeStatus(w, http.StatusRequestTimeout, err.Error())
	default:
		writeStatus(w, http.StatusBadRequest, err.Error())
	}
}

func isStateError(err error) bool {
	var transition *domain.ErrInvalidTransition
	return errors.As(err, &transition)
}

// logRequests records each request's outcome.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logger.Debug("request",
			"method", r.Method, "path", r.URL.Path,
			"status", recorder.status, "duration", time.Since(started))
	})
}

// statusRecorder captures the status code for logging while preserving the
// streaming behaviour the event feed depends on.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying writer so server-sent events keep working
// through the logging wrapper.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack forwards to the underlying writer so the websocket upgrade can take
// over the connection. Without this the wrapper silently removes the
// capability and every upgrade fails.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("api: the underlying writer does not support hijacking")
	}
	return hijacker.Hijack()
}
