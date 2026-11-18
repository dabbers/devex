package api

import (
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/install"
	"github.com/dabbers/devex/internal/store"
)

// Installing an app, and seeing what is installed.
//
// This is the surface the whole platform exists to offer: a customer picks
// something from a catalog and a minute later it is running on their own
// address, with its own database, and they never learned that any of it was
// git, Postgres or a process on a machine.

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"apps": []any{}})
		return
	}

	apps, problems := s.installer.Catalog().List()
	listed := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		listed = append(listed, map[string]any{
			"app":     app.Spec.App,
			"name":    app.Spec.Name,
			"version": app.Spec.Version,
			"license": app.Spec.License,
			// What it will ask the platform for, so the catalog page can say
			// what an installation involves before anyone commits to one.
			"requires": app.Spec.Requires,
			"secrets":  app.Spec.Secrets,
		})
	}
	// An app whose manifest does not validate is left out rather than offered
	// broken, and the reason is logged: an app nobody can install is better
	// than one that fails halfway through installing.
	for _, problem := range problems {
		s.logger.Warn("an app in the catalog does not satisfy the contract", "error", problem)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": listed})
}

// installRequest asks for an app to be installed.
type installRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`
}

func (s *Server) createInstallation(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no app catalog")
		return
	}
	var req installRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.App) == "" {
		writeStatus(w, http.StatusBadRequest, "say which app to install")
		return
	}

	// Deliberately synchronous for now. Installing provisions a database,
	// seeds a repository, compiles the app and starts it, which is a minute
	// rather than an hour -- and a request that returns when the thing is
	// actually running is worth more than one that returns immediately and
	// leaves the customer watching a spinner.
	installation, err := s.installer.Install(r.Context(), install.Request{
		UserID: s.userID(r),
		App:    req.App,
		Name:   req.Name,
	})
	if err != nil {
		// A failed install still produced a record, so the caller gets it
		// alongside the reason: the alternative is an installation that
		// exists, is broken, and is invisible.
		if installation != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"installation": installation,
				"error":        err.Error(),
			})
			return
		}
		writeError(w, err)
		return
	}

	s.audit(r.Context(), s.userID(r), &domain.Event{
		RepoID: installation.RepoID,
		Type:   domain.EventWorkspaceStarted,
		Message: "installed " + installation.App + " " + installation.AppVersion +
			" as " + installation.Name,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"installation": installation})
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request) {
	installations, err := s.store.ListInstallations(r.Context(), store.InstallationFilter{
		UserID: s.userID(r),
		App:    r.URL.Query().Get("app"),
		Limit:  intParam(r, "limit", 100),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"installations": installations})
}

func (s *Server) getInstallation(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"installation": scopedInstallation(r)})
}
