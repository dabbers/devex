package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/proxy/hostmux"
)

// EdgeSignIn is the in-process edge, seen from the panel: given somebody the
// panel has identified, it returns the link that signs them in at one app.
type EdgeSignIn interface {
	Handoff(host, state string, user *domain.User) (string, error)
}

type edgeSignInRequest struct {
	Host  string `json:"host"`
	State string `json:"state"`
}

// edgeSignIn is the panel's half of signing in at an app: the visitor arrived
// from that app's edge with a hostname and a state, the panel knows who they
// are, and this turns that into a one-time link back.
//
// It attests who somebody is and nothing more. Whether they may see the app
// is the edge's call, made against the app's members on every request, so a
// link for an app you are not a member of signs you in to a polite refusal.
func (s *Server) edgeSignIn(w http.ResponseWriter, r *http.Request) {
	if s.edge == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment does not sign people in at apps")
		return
	}
	var req edgeSignInRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	who := user(r)
	if who == nil {
		writeStatus(w, http.StatusUnauthorized, "sign in first")
		return
	}
	link, err := s.edge.Handoff(strings.TrimSpace(req.Host), strings.TrimSpace(req.State), who)
	switch {
	case errors.Is(err, hostmux.ErrUnknownHost):
		writeStatus(w, http.StatusNotFound, "no app is published at "+req.Host)
		return
	case err != nil:
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": link})
}

// setInstallationAccess changes who an installation lets in, and republishes
// so the edge enforces it from the next request.
func (s *Server) setInstallationAccess(w http.ResponseWriter, r *http.Request) {
	var access domain.Access
	if !decodeJSON(w, r, &access) {
		return
	}
	if err := access.Validate(); err != nil {
		writeStatus(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "domain: "))
		return
	}
	if access.Mode != domain.AccessHybrid {
		access.Public = nil
	}
	installation := scopedInstallation(r)
	installation.Access = access
	if err := s.store.UpdateInstallation(r.Context(), installation); err != nil {
		writeError(w, err)
		return
	}
	if s.preview != nil {
		if err := s.preview.Publish(r.Context()); err != nil {
			// Recorded but not yet enforced is worth saying loudly: the owner
			// thinks a door is shut that is still open.
			writeStatus(w, http.StatusBadGateway, "saved, but the edge could not be updated: "+err.Error())
			return
		}
	}
	s.audit(r.Context(), s.userID(r), &domain.Event{
		RepoID:  installation.RepoID,
		Type:    domain.EventInstallationAccess,
		Message: installation.Name + " is now " + string(access.Mode),
	})
	writeJSON(w, http.StatusOK, map[string]any{"installation": installation})
}
