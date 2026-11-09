package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// createWorkspaceRequest opens an environment to work in.
type createWorkspaceRequest struct {
	RepoID    string `json:"repo_id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	// Branch is what gets checked out. Empty means the repo's default branch:
	// a workspace is somewhere you work, and working on the default branch is
	// a legitimate thing to want, unlike for a fork.
	Branch string `json:"branch"`
	// Start brings it up immediately rather than leaving it stopped.
	Start bool `json:"start"`
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	filter := store.WorkspaceFilter{
		UserID: s.userID(r),
		RepoID: r.URL.Query().Get("repo"),
		// The scratch workspaces a plan made for its forks are shown only when
		// asked for: this list is "places you work", not every machine.
		IncludeEphemeral: r.URL.Query().Get("all") == "true",
	}
	workspaces, err := s.store.ListWorkspaces(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": workspaces})
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createWorkspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()

	// The repo arrives in the body, where the route pattern cannot see it.
	repo, err := s.ownedRepo(r, req.RepoID)
	if err != nil {
		writeError(w, err)
		return
	}

	project, err := s.resolveProject(ctx, repo.ID, req.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeStatus(w, http.StatusBadRequest, "a workspace needs a name")
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = repo.DefaultBranch
	}

	ws := &domain.Workspace{
		UserID: s.userID(r), RepoID: repo.ID, ProjectID: project.ID,
		Name: name, Branch: branch,
	}
	if err := s.store.CreateWorkspace(ctx, ws); err != nil {
		writeError(w, err)
		return
	}
	s.audit(ctx, s.userID(r), &domain.Event{
		RepoID: repo.ID, WorkspaceID: ws.ID,
		Type:    domain.EventWorkspaceCreated,
		Message: "created the workspace " + ws.Name,
		Data:    map[string]any{"branch": ws.Branch, "project": project.Name},
	})

	if req.Start {
		s.startInBackground(ws, s.userID(r))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": ws})
}

// resolveProject picks the project a workspace is for, defaulting to the
// repo's only one when it has just one.
func (s *Server) resolveProject(ctx context.Context, repoID, projectID string) (*domain.Project, error) {
	if projectID != "" {
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if project.RepoID != repoID {
			return nil, fmt.Errorf("project %s does not belong to this repository", projectID)
		}
		return project, nil
	}

	projects, err := s.store.ListProjects(ctx, repoID)
	if err != nil {
		return nil, err
	}
	switch len(projects) {
	case 0:
		return nil, errors.New("this repository has no projects yet; run discovery first")
	case 1:
		return projects[0], nil
	default:
		// Picking one would be a guess, and the wrong guess means a workspace
		// running the wrong services.
		return nil, fmt.Errorf("this repository has %d projects; say which one", len(projects))
	}
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}

	services, err := s.store.ListServices(ctx, ws.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	response := map[string]any{"workspace": ws, "services": services}

	if s.preview != nil {
		routes, err := s.store.ListPreviewRoutesFor(ctx, ws.ID)
		if err != nil {
			writeError(w, err)
			return
		}
		byID := make(map[string]*domain.Service, len(services))
		for _, service := range services {
			byID[service.ID] = service
		}
		previews := make([]map[string]any, 0, len(routes))
		for _, route := range routes {
			entry := map[string]any{
				"service_id": route.ServiceID,
				"hostname":   route.Hostname,
				"url":        s.preview.URL(route),
				"port":       route.UpstreamPort,
			}
			if service := byID[route.ServiceID]; service != nil {
				entry["service"] = service.Name
				entry["kind"] = service.Kind
				entry["primary"] = service.Primary
			}
			previews = append(previews, entry)
		}
		response["previews"] = previews
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) startWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.workspaces == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment cannot start workspaces")
		return
	}
	if ws.State.Terminal() {
		writeStatus(w, http.StatusConflict, "this workspace is archived")
		return
	}
	if ws.State == domain.WorkspaceStarting || ws.State == domain.WorkspaceRunning {
		writeJSON(w, http.StatusOK, map[string]any{"workspace": ws})
		return
	}

	s.startInBackground(ws, s.userID(r))
	// Accepted, not OK: booting a machine and starting services takes long
	// enough that holding the request open would time out in a browser. The
	// state changes arrive on the activity stream.
	writeJSON(w, http.StatusAccepted, map[string]any{"workspace": ws})
}

// startInBackground brings a workspace up without holding the request open.
func (s *Server) startInBackground(ws *domain.Workspace, userID string) {
	go func() {
		// Deliberately not the request's context: the browser navigating away
		// must not kill a machine that is half booted.
		ctx := context.WithoutCancel(context.Background())

		if _, err := s.workspaces.Start(ctx, ws); err != nil {
			s.logger.Error("could not start workspace", "workspace", ws.ID, "error", err)
			s.auditAs(ctx, userID, domain.ActorOrchestrator, &domain.Event{
				RepoID: ws.RepoID, WorkspaceID: ws.ID,
				Type:    domain.EventError,
				Message: "could not start " + ws.Name + ": " + err.Error(),
			})
			return
		}
		s.auditAs(ctx, userID, domain.ActorOrchestrator, &domain.Event{
			RepoID: ws.RepoID, WorkspaceID: ws.ID,
			Type:    domain.EventWorkspaceStarted,
			Message: ws.Name + " is running",
			Data:    map[string]any{"instance": ws.InstanceID, "preview_url": ws.PreviewURL},
		})
	}()
}

func (s *Server) stopWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.workspaces == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment cannot stop workspaces")
		return
	}
	if err := s.workspaces.Stop(ctx, ws); err != nil {
		writeError(w, err)
		return
	}
	s.audit(ctx, s.userID(r), &domain.Event{
		RepoID: ws.RepoID, WorkspaceID: ws.ID,
		Type:    domain.EventWorkspaceStopped,
		Message: "stopped " + ws.Name,
		// The machine is deliberately left alone, and saying so here is what
		// stops "stopped" being read as "destroyed".
		Data: map[string]any{"instance_kept": ws.InstanceID},
	})
	writeJSON(w, http.StatusOK, map[string]any{"workspace": ws})
}

func (s *Server) archiveWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if ws.State.Occupies() {
		writeStatus(w, http.StatusConflict, "stop this workspace before archiving it")
		return
	}
	archived, err := s.store.TransitionWorkspace(ctx, ws.ID, ws.State, domain.WorkspaceArchived,
		"archived by the user")
	if err != nil {
		writeStatus(w, http.StatusConflict, err.Error())
		return
	}
	s.audit(ctx, s.userID(r), &domain.Event{
		RepoID: ws.RepoID, WorkspaceID: ws.ID,
		Type:    domain.EventWorkspaceArchived,
		Message: "archived " + ws.Name,
	})
	writeJSON(w, http.StatusOK, map[string]any{"workspace": archived})
}

// workspaceForUser loads the workspace named in the path and refuses one that
// belongs to somebody else.
func (s *Server) workspaceForUser(r *http.Request) (*domain.Workspace, error) {
	// The tenancy gate loaded and authorized it before this handler ran, so
	// there is nothing left to check here; an absent one means the route was
	// registered without a {workspace} in its pattern.
	ws := scopedWorkspace(r)
	if ws == nil {
		return nil, store.ErrNotFound
	}
	return ws, nil
}

// workspaceServices reports what each service is doing right now.
//
// Live rather than stored: a recorded status goes stale the moment a dev
// server falls over, and "it says running" when it is not is worse than a
// slightly slower page.
func (s *Server) workspaceServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	services, err := s.store.ListServices(ctx, ws.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}

	response := map[string]any{"services": services}
	if s.workspaces != nil && ws.InstanceID != "" {
		status, err := s.workspaces.Status(ctx, ws, services)
		if err != nil {
			// The definitions are still worth returning: a machine that
			// cannot be reached is itself the answer to "what is running".
			response["status_error"] = err.Error()
		} else {
			response["status"] = status
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// workspaceServiceLogs returns the tail of one service's output, which is
// where the reason a service would not start actually is.
func (s *Server) workspaceServiceLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.workspaceForUser(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.workspaces == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment cannot read service logs")
		return
	}

	name := r.PathValue("service")
	// Only a service this project declares, so the path cannot be used to
	// read an arbitrary file off the machine.
	services, err := s.store.ListServices(ctx, ws.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	known := false
	for _, service := range services {
		if service.Name == name {
			known = true
			break
		}
	}
	if !known {
		writeStatus(w, http.StatusNotFound, "this project has no service called "+name)
		return
	}

	logs, err := s.workspaces.Logs(ctx, ws, name, intParam(r, "lines", 200))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": name, "logs": logs})
}
