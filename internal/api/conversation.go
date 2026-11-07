package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// sendMessageRequest is one thing asked of the agent.
type sendMessageRequest struct {
	Content string `json:"content"`
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	messages, err := s.store.ListMessages(r.Context(), ws.ID, intParam(r, "limit", 200))
	if err != nil {
		writeError(w, err)
		return
	}
	if messages == nil {
		messages = []*domain.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// sendMessage asks the agent for something in this workspace.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req sendMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeStatus(w, http.StatusBadRequest, "say something for the agent to do")
		return
	}

	ctx := r.Context()
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.agent == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no coding agent configured")
		return
	}
	// The agent works on the machine, so there has to be one. Starting the
	// workspace here would hide a minutes-long boot behind what looks like
	// sending a message.
	if ws.State != domain.WorkspaceRunning || ws.InstanceID == "" {
		writeStatus(w, http.StatusConflict, "start this workspace before asking the agent for anything")
		return
	}

	prompt := &domain.Message{WorkspaceID: ws.ID, UserID: s.userID(r), Content: content}
	reply, err := s.store.StartAgentTurn(ctx, prompt)
	switch {
	case errors.Is(err, store.ErrAgentBusy):
		// Two runs on one machine would be changing the same files underneath
		// each other.
		writeStatus(w, http.StatusConflict, "the agent is already working in this workspace")
		return
	case err != nil:
		writeError(w, err)
		return
	}

	s.audit(ctx, s.userID(r), &domain.Event{
		RepoID: ws.RepoID, WorkspaceID: ws.ID,
		Type:    domain.EventAgentMessage,
		Message: truncate(content, 200),
	})

	s.runAgentTurn(ws, prompt, reply)
	// Accepted: the agent takes minutes, and the reply arrives on the event
	// stream rather than on this request.
	writeJSON(w, http.StatusAccepted, map[string]any{"message": prompt, "reply": reply})
}

// runAgentTurn works the agent in the background and records what it said.
func (s *Server) runAgentTurn(ws *domain.Workspace, prompt, reply *domain.Message) {
	go func() {
		// Deliberately not the request's context: the browser navigating away
		// must not abandon a run that is already changing the machine.
		ctx := context.WithoutCancel(context.Background())

		services, err := s.store.ListServices(ctx, ws.ProjectID)
		if err != nil {
			s.failTurn(ctx, ws, reply, err)
			return
		}
		previews, err := s.workspacePreviews(ctx, ws, services)
		if err != nil {
			s.failTurn(ctx, ws, reply, err)
			return
		}
		repo, err := s.store.GetRepo(ctx, ws.RepoID)
		if err != nil {
			s.failTurn(ctx, ws, reply, err)
			return
		}
		project, err := s.store.GetProject(ctx, ws.ProjectID)
		if err != nil {
			s.failTurn(ctx, ws, reply, err)
			return
		}
		history, err := s.store.ListMessages(ctx, ws.ID, 200)
		if err != nil {
			s.failTurn(ctx, ws, reply, err)
			return
		}

		// Partial text is written back to the row so the interface can show
		// the agent working rather than a spinner. Throttled, because the
		// alternative is a database write per token.
		progress := newProgressWriter(ctx, s, reply)

		res, err := s.agent.Run(ctx, agent.Request{
			InstanceID: ws.InstanceID,
			Prompt: agent.BuildConversationPrompt(
				ws, repo, project, services, previews, prompt.Content, history),
			// The workspace's own conversation, so each message continues the
			// last instead of starting from an empty context -- which is the
			// thing that would make a chat useless.
			Session: &ws.AgentSession,
			OnText:  progress.write,
		})
		if err != nil {
			s.failTurn(ctx, ws, reply, err)
			return
		}
		// Persist the conversation immediately: the identifier and what has
		// been accounted for it are the workspace's, not the run's.
		if err := s.store.UpdateWorkspace(ctx, ws); err != nil {
			s.logger.Error("could not record the workspace conversation",
				"workspace", ws.ID, "error", err)
		}

		reply.Content = res.Output
		reply.Usage = res.Usage
		reply.State = domain.MessageDone
		if res.Failed {
			reply.State = domain.MessageFailed
			reply.StateReason = "the agent reported a failure"
		}
		if res.Escalation != nil {
			// An escalation is an answer, not a failure: the agent is asking
			// rather than refusing, and the question is the content.
			reply.State = domain.MessageDone
			reply.Content = res.Escalation.Message
		}
		if err := s.store.FinishMessage(ctx, reply); err != nil {
			s.logger.Error("could not record the agent's reply", "message", reply.ID, "error", err)
			return
		}

		s.auditAs(ctx, ws.UserID, domain.ActorAgent, &domain.Event{
			RepoID: ws.RepoID, WorkspaceID: ws.ID,
			Type:    domain.EventAgentMessage,
			Message: truncate(reply.Content, 200),
			Data: map[string]any{
				"cost_usd":       res.Usage.CostUSD,
				"tokens":         res.Usage.InputTokens + res.Usage.OutputTokens,
				"resumed":        res.Resumed,
				"usage_reported": res.UsageReported,
			},
		})
	}()
}

// failTurn records that a turn could not be completed.
//
// The row has to be closed either way: left running, the workspace shows an
// agent working forever and refuses every later message.
func (s *Server) failTurn(ctx context.Context, ws *domain.Workspace, reply *domain.Message, cause error) {
	s.logger.Error("agent turn failed", "workspace", ws.ID, "error", cause)
	reply.State = domain.MessageFailed
	reply.StateReason = cause.Error()
	if reply.Content == "" {
		reply.Content = "This could not be run: " + cause.Error()
	}
	if err := s.store.FinishMessage(ctx, reply); err != nil {
		s.logger.Error("could not record a failed turn", "message", reply.ID, "error", err)
	}
}

// workspacePreviews assembles what the agent is told it can open.
func (s *Server) workspacePreviews(ctx context.Context, ws *domain.Workspace, services []*domain.Service) (agent.Previews, error) {
	previews := agent.Previews{URLs: map[string]string{}, Primary: ws.PreviewURL}
	if s.preview == nil {
		return previews, nil
	}
	routes, err := s.store.ListPreviewRoutesFor(ctx, ws.ID)
	if err != nil {
		return agent.Previews{}, err
	}
	byID := make(map[string]*domain.Service, len(services))
	for _, service := range services {
		byID[service.ID] = service
	}
	for _, route := range routes {
		if service := byID[route.ServiceID]; service != nil {
			previews.URLs[service.Name] = s.preview.URL(route)
		}
	}
	return previews, nil
}

// truncate shortens text for an audit entry, which is a one-liner on the feed
// rather than the whole message.
func truncate(text string, limit int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(clean) <= limit {
		return clean
	}
	return clean[:limit] + "…"
}

// progressInterval bounds how often a turn in flight is written back.
//
// Every update carries the whole answer so far, so a dropped one costs
// nothing: the next write supersedes it. A second is slow enough to be a
// handful of writes over a long run and fast enough that the text visibly
// moves.
const progressInterval = time.Second

// progressWriter records the agent's answer as it is written.
//
// It holds no lock of its own because the agent calls it from one goroutine,
// in order. What it does guard against is writing on every token: the callback
// fires per delta, which is far too often to put in a database.
type progressWriter struct {
	ctx     context.Context
	server  *Server
	message *domain.Message
	last    time.Time
}

func newProgressWriter(ctx context.Context, server *Server, message *domain.Message) *progressWriter {
	return &progressWriter{ctx: ctx, server: server, message: message}
}

// write records the answer so far, at most once per interval.
func (p *progressWriter) write(text string) {
	if text == "" || time.Since(p.last) < progressInterval {
		return
	}
	p.last = time.Now()

	// Still running: this is the answer so far, not the answer. Writing a
	// state here would mark the turn finished while the agent is mid-sentence.
	partial := *p.message
	partial.Content = text
	partial.State = domain.MessageRunning
	if err := p.server.store.FinishMessage(p.ctx, &partial); err != nil {
		// Losing progress is cosmetic; the turn still completes and the final
		// write carries everything. Worth a line, not worth stopping for.
		p.server.logger.Debug("could not record partial agent output",
			"message", p.message.ID, "error", err)
	}
}
