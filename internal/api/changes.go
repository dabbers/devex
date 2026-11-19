package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// Changing an installation without being able to break it.
//
// This is the half of the pitch that the install path cannot carry on its
// own. Anybody can start a copy of an app; the reason to keep paying for one
// is that asking for a change to it is not a gamble. So the operations here
// are deliberately not "edit the running thing": a change gets its own branch,
// its own machine and its own copy of the data, is checked where nothing a
// customer depends on is watching, and only then becomes the live revision --
// with the previous one still recorded, so going back is one request.
//
// Nothing here is the agent yet. These are the handles the agent, the UI and
// a person with curl all reach for, and they exist first because an agent
// driving a flow nobody can inspect is not something to debug later.

// changeRequest asks to open a change against an installation.
type changeRequest struct {
	Title string `json:"title"`
}

// openChange starts a change against an installation.
func (s *Server) openChange(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}
	var req changeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeStatus(w, http.StatusBadRequest, "say what the change is for")
		return
	}

	// Synchronous, like installing, and for the same reason: it forks the
	// data, starts a machine and brings up a preview, which is a minute. A
	// request that returns once there is somewhere to work is worth more than
	// one that returns an id and leaves the caller polling to find out whether
	// it worked.
	change, err := s.installer.OpenChange(r.Context(), scopedInstallation(r), req.Title)
	if err != nil {
		// A change that failed while preparing still exists and still holds a
		// database and a workspace. Returning it means the caller can see the
		// reason and abandon it, rather than it being invisible and leaked.
		if change != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"change": change,
				"error":  err.Error(),
			})
			return
		}
		writeError(w, err)
		return
	}

	s.audit(r.Context(), s.userID(r), &domain.Event{
		RepoID:  change.InstallationID,
		Type:    domain.EventWorkspaceStarted,
		Message: "opened change " + change.ID + ": " + change.Title,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"change": change})
}

// listChanges returns an installation's changes, newest first.
func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	changes, err := s.store.ListChanges(r.Context(), scopedInstallation(r).ID, intParam(r, "limit", 50))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

// getChange returns one change.
func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"change": scopedChange(r)})
}

// checkChange runs the change's own suite and the browser harness over its
// preview, and records what they found.
func (s *Server) checkChange(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}

	change, err := s.installer.Check(r.Context(), scopedChange(r))
	if err != nil {
		writeError(w, err)
		return
	}
	// A change that failed its checks is a successful answer to "did this
	// work": the caller asked to be told, and it is being told. The reason is
	// in the recorded results rather than in an HTTP status.
	writeJSON(w, http.StatusOK, map[string]any{"change": change})
}

// promoteChange makes a change the installation's live revision.
func (s *Server) promoteChange(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}

	change := scopedChange(r)
	installation, err := s.installer.Promote(r.Context(), change)
	if err != nil {
		// The change comes back with the refusal. When promoting found a
		// conflict, or brought the change up to date and its checks then
		// failed, the change's new state and its recorded files are the
		// answer; an error string alone would send the caller off to fetch
		// them.
		if errors.Is(err, store.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"change": change, "error": err.Error()})
			return
		}
		writeError(w, err)
		return
	}

	s.audit(r.Context(), s.userID(r), &domain.Event{
		RepoID:  installation.ID,
		Type:    domain.EventWorkspaceStarted,
		Message: "promoted change " + scopedChange(r).ID + " to " + installation.Revision,
	})
	writeJSON(w, http.StatusOK, map[string]any{"installation": installation})
}

// resolveChange hands a conflicted change to its own agent, in its own
// workspace, with a message saying what went live in the meantime and which
// files conflict. The reply arrives as any other agent turn does; once the
// agent has committed the resolution, the change is checked and promoted as
// usual.
func (s *Server) resolveChange(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}
	change := scopedChange(r)
	brief, err := s.installer.ConflictBrief(r.Context(), change)
	if err != nil {
		writeError(w, err)
		return
	}
	ws, err := s.store.GetWorkspace(r.Context(), change.WorkspaceID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.askAgent(w, r, ws, brief, map[string]any{"change": change})
}

// abandonChange throws a change away, with its workspace and its copy of the
// data.
func (s *Server) abandonChange(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}

	if err := s.installer.Abandon(r.Context(), scopedChange(r)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rollbackInstallation puts the previous revision back.
//
// Separate from the change operations because it is the one that gets reached
// for in a hurry: it names only the installation, needs no knowledge of which
// change caused the problem, and works whether or not that change still
// exists.
func (s *Server) rollbackInstallation(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}

	installation, err := s.installer.Rollback(r.Context(), scopedInstallation(r))
	if err != nil {
		writeError(w, err)
		return
	}

	s.audit(r.Context(), s.userID(r), &domain.Event{
		RepoID:  installation.ID,
		Type:    domain.EventWorkspaceStarted,
		Message: "rolled " + installation.Name + " back to " + installation.Revision,
	})
	writeJSON(w, http.StatusOK, map[string]any{"installation": installation})
}
