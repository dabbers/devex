package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/claudeauth"
	"github.com/dabbers/devex/internal/domain"
)

// ClaudeTokens is what the API needs from Claude credential resolution: what
// an account's runs would authenticate with, without the token itself.
type ClaudeTokens interface {
	Status(ctx context.Context, userID string) (claudeauth.Status, error)
}

// The account's own Claude token.
//
// The customer brings the subscription, so the token is theirs to set, and is
// always the signed-in account's: there is no path value naming another
// account, so there is no way to address somebody else's. It is written once
// and never read back -- the status says whether one is set and what runs
// would use, and no response, log line or audit entry carries the value.

type putClaudeTokenRequest struct {
	Token string `json:"token"`
}

func (s *Server) claudeStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.claudeTokenStatus(r.Context(), s.userID(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claude": status})
}

func (s *Server) putClaudeToken(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	var req putClaudeTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Trimmed first: a pasted token very often brings its newline along.
	token := strings.TrimSpace(req.Token)
	if err := claudeauth.Validate(token); err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	userID := s.userID(r)
	if err := s.vault.SetCredential(r.Context(), userID, claudeauth.Kind, token); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r.Context(), userID, &domain.Event{
		Type: domain.EventClaudeTokenSet, Message: "Claude token set",
	})
	s.claudeStatus(w, r)
}

func (s *Server) deleteClaudeToken(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeStatus(w, http.StatusServiceUnavailable, "secrets are not configured")
		return
	}
	userID := s.userID(r)
	if err := s.vault.DeleteCredential(r.Context(), userID, claudeauth.Kind); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r.Context(), userID, &domain.Event{
		Type: domain.EventClaudeTokenDeleted, Message: "Claude token removed",
	})
	w.WriteHeader(http.StatusNoContent)
}

// claudeTokenStatus asks the resolver when there is one, and otherwise
// answers from the vault alone, which cannot know about a fallback and so
// reports none.
func (s *Server) claudeTokenStatus(ctx context.Context, userID string) (claudeauth.Status, error) {
	if s.claude != nil {
		return s.claude.Status(ctx, userID)
	}
	if s.vault == nil {
		return claudeauth.Status{Source: claudeauth.SourceNone, Blocked: "secrets are not configured"}, nil
	}
	resolver, err := claudeauth.New(claudeauth.Options{Vault: s.vault})
	if err != nil {
		return claudeauth.Status{}, err
	}
	return resolver.Status(ctx, userID)
}
