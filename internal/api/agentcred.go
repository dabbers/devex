package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/agentauth"
	"github.com/dabbers/devex/internal/domain"
)

// AgentCreds is what the API needs from agent credential resolution: what an
// account's runs would authenticate with, without the credential itself.
type AgentCreds interface {
	Status(ctx context.Context, userID string) (agentauth.Status, error)
}

// The account's own agent subscription credential.
//
// The customer brings the subscription -- Claude today, Codex or Grok once
// they have runners -- so the credential is theirs to set, and is always the
// signed-in account's: there is no path value naming another account, so
// there is no way to address somebody else's. There is one per account, and
// setting one for another provider replaces it. It is written once and never
// read back: the status says which provider is set and what runs would use,
// and no response, log line or audit entry carries the value.

type putAgentCredentialRequest struct {
	Provider   domain.AgentProvider `json:"provider"`
	Credential string               `json:"credential"`
}

// agentCredentialResponse is the one shape GET and PUT answer with, so the
// panel renders the same thing after saving as after loading.
type agentCredentialResponse struct {
	Agent     agentauth.Status     `json:"agent"`
	Providers []agentauth.Provider `json:"providers"`
}

func (s *Server) agentCredentialStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.agentStatus(r.Context(), s.userID(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentCredentialResponse{Agent: status, Providers: agentauth.Providers})
}

func (s *Server) putAgentCredential(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	var req putAgentCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	provider := domain.AgentProvider(strings.ToLower(strings.TrimSpace(string(req.Provider))))
	// Trimmed first: a pasted token very often brings its newline along.
	credential := strings.TrimSpace(req.Credential)
	if err := agentauth.Validate(provider, credential); err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	userID := s.userID(r)
	if err := s.vault.SetCredential(r.Context(), userID, agentauth.Kind, string(provider), credential); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r.Context(), userID, &domain.Event{
		Type:    domain.EventAgentCredentialSet,
		Message: "agent credential set for " + string(provider),
		Data:    map[string]any{"provider": provider},
	})
	s.agentCredentialStatus(w, r)
}

func (s *Server) deleteAgentCredential(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	userID := s.userID(r)
	if err := s.vault.DeleteCredential(r.Context(), userID, agentauth.Kind); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r.Context(), userID, &domain.Event{
		Type: domain.EventAgentCredentialDeleted, Message: "agent credential removed",
	})
	w.WriteHeader(http.StatusNoContent)
}

// agentStatus asks the resolver when there is one, and otherwise answers
// from the vault alone, which cannot know about a fallback and so reports
// none.
func (s *Server) agentStatus(ctx context.Context, userID string) (agentauth.Status, error) {
	if s.agentCreds != nil {
		return s.agentCreds.Status(ctx, userID)
	}
	if s.vault == nil {
		return agentauth.Status{Source: agentauth.SourceNone, Blocked: "secrets are not configured"}, nil
	}
	resolver, err := agentauth.New(agentauth.Options{Vault: s.vault})
	if err != nil {
		return agentauth.Status{}, err
	}
	return resolver.Status(ctx, userID)
}
