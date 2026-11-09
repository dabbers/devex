package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// Tenancy is enforced here rather than in each handler.
//
// Authentication says who is asking; this says what they are allowed to ask
// about. Those are different questions, and the second one was being answered
// by individual handlers remembering to compare a user id -- which is exactly
// the kind of check a new route forgets. A forgotten check on a repo leaks a
// listing; a forgotten check on a fork hands out a shell on somebody else's
// machine.
//
// So the gate is derived from the route pattern rather than written by hand:
// a pattern naming {repo}, {task}, {fork} or {workspace} is authorized before
// its handler runs, and a route cannot opt out by omission. The resolved
// object is left on the request, so the handler reads it instead of fetching
// it again.
//
// Something belonging to somebody else is reported as missing, not forbidden.
// A 403 confirms the id exists, which turns any id-shaped guess into an
// oracle for what other people have.

// scope names a kind of owned resource, matching the path value that carries
// its id.
type scope string

const (
	repoScope      scope = "repo"
	taskScope      scope = "task"
	forkScope      scope = "fork"
	workspaceScope scope = "workspace"
)

// ownedScopes is every path value that names something with an owner. Adding
// an owned resource means adding it here; nothing else needs to change.
var ownedScopes = []scope{repoScope, taskScope, forkScope, workspaceScope}

// scopeKey types the context keys so nothing else can collide with them.
type scopeKey scope

// router registers routes and records them, so the route table itself can be
// tested rather than a remembered copy of it.
type router struct {
	mux    *http.ServeMux
	server *Server
	routes []string
}

func newRouter(s *Server) *router {
	return &router{mux: http.NewServeMux(), server: s}
}

// handle registers a route, gating it on ownership when its pattern names a
// resource that has an owner.
func (rt *router) handle(pattern string, handler http.HandlerFunc) {
	rt.routes = append(rt.routes, pattern)
	if scopes := scopesIn(pattern); len(scopes) > 0 {
		handler = rt.server.authorizeScopes(scopes, handler)
	}
	rt.mux.HandleFunc(pattern, handler)
}

// scopesIn reports which owned resources a route pattern names.
func scopesIn(pattern string) []scope {
	var found []scope
	for _, named := range ownedScopes {
		if strings.Contains(pattern, "{"+string(named)+"}") {
			found = append(found, named)
		}
	}
	return found
}

// authorizeScopes resolves every owned resource in the path and refuses the
// request unless all of them belong to the caller.
func (s *Server) authorizeScopes(scopes []scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		caller := s.userID(r)

		for _, named := range scopes {
			id := r.PathValue(string(named))
			if id == "" {
				// A pattern names it, so the mux cannot have matched without
				// it. Refusing beats guessing.
				writeStatus(w, http.StatusBadRequest, "this request names no "+string(named))
				return
			}

			resource, owner, err := s.resolveScope(ctx, named, id)
			if err != nil {
				writeError(w, err)
				return
			}
			if owner != caller || caller == "" {
				s.logger.Warn("refused a request for somebody else's resource",
					"scope", named, "id", id, "caller", caller)
				writeError(w, store.ErrNotFound)
				return
			}
			ctx = context.WithValue(ctx, scopeKey(named), resource)
		}
		next(w, r.WithContext(ctx))
	}
}

// resolveScope loads one owned resource and reports who owns it.
func (s *Server) resolveScope(ctx context.Context, named scope, id string) (any, string, error) {
	switch named {
	case repoScope:
		repo, err := s.store.GetRepo(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return repo, repo.UserID, nil
	case taskScope:
		task, err := s.store.GetTask(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return task, task.UserID, nil
	case forkScope:
		fork, err := s.store.GetFork(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return fork, fork.UserID, nil
	case workspaceScope:
		ws, err := s.store.GetWorkspace(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return ws, ws.UserID, nil
	default:
		// Unreachable while ownedScopes and this switch agree, and a scope
		// with no resolver must not pass unchecked.
		return nil, "", store.ErrNotFound
	}
}

// ownedRepo authorizes a repo named in a request body rather than in the path.
//
// The gate works from the route pattern, so it cannot see a repo id that
// arrives as JSON. Creating a task or a workspace does exactly that, and
// without this a caller could start agents on a repo that is not theirs.
func (s *Server) ownedRepo(r *http.Request, repoID string) (*domain.Repo, error) {
	repo, err := s.store.GetRepo(r.Context(), repoID)
	if err != nil {
		return nil, err
	}
	if repo.UserID != s.userID(r) || repo.UserID == "" {
		// Missing rather than forbidden, for the reason at the top of this
		// file.
		return nil, store.ErrNotFound
	}
	return repo, nil
}

// scopedRepo returns the repo the gate authorized for this request.
//
// The gate has already loaded and checked it, so a handler reading one of
// these is reading something that has been authorized by construction.
func scopedRepo(r *http.Request) *domain.Repo {
	repo, _ := r.Context().Value(scopeKey(repoScope)).(*domain.Repo)
	return repo
}

// scopedTask returns the task the gate authorized for this request.
func scopedTask(r *http.Request) *domain.Task {
	task, _ := r.Context().Value(scopeKey(taskScope)).(*domain.Task)
	return task
}

// scopedFork returns the fork the gate authorized for this request.
func scopedFork(r *http.Request) *domain.Fork {
	fork, _ := r.Context().Value(scopeKey(forkScope)).(*domain.Fork)
	return fork
}

// scopedWorkspace returns the workspace the gate authorized for this request.
func scopedWorkspace(r *http.Request) *domain.Workspace {
	ws, _ := r.Context().Value(scopeKey(workspaceScope)).(*domain.Workspace)
	return ws
}
