package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/domain"
)

// userKey carries the resolved user through a request.
type userKey struct{}

// requireUser wraps a handler so it only runs for an identified user, and
// makes that user available to it.
//
// It is applied to the whole mux rather than route by route, with an explicit
// list of exceptions. The alternative -- opting each route in -- means a route
// added later is unprotected by default, and the thing being protected here
// includes a shell on the machine.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authn == nil || isUnauthenticatedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		identity, err := s.authn.Identify(r.Context(), r)
		if err != nil {
			s.rejectUnauthenticated(w, r, err)
			return
		}

		// First sign-in creates the dabberz user. Everything is already scoped
		// by user id, so this is the only place identity has to be translated.
		user, err := s.store.EnsureUserByExternalID(r.Context(), identity.Subject, identity.Email)
		if err != nil {
			s.logger.Error("could not resolve the signed-in user",
				"subject", identity.Subject, "error", err)
			writeStatus(w, http.StatusForbidden, "your account could not be resolved")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

// isUnauthenticatedPath reports whether a path is deliberately open.
//
// Kept to the smallest possible set, each with a reason:
//
//   - /healthz is how a platform decides whether this process is alive, and a
//     health check cannot hold a credential.
//   - /v1/auth/config tells the browser which provider to sign in against. It
//     is read before anyone is signed in, by definition, and contains only the
//     publishable configuration.
//
// The UI's own assets are NOT in this list. They are served to anyone, which
// is harmless -- the page is a shell that fetches everything it shows -- but
// the fetches behind it are not, so the page renders a sign-in prompt instead
// of data.
func isUnauthenticatedPath(path string) bool {
	switch path {
	case "/healthz", "/v1/auth/config":
		return true
	}
	// Static assets and the page itself. Anything under /v1 is data and needs
	// a user.
	return !strings.HasPrefix(path, "/v1/")
}

// rejectUnauthenticated answers a request that could not be identified.
func (s *Server) rejectUnauthenticated(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusUnauthorized
	message := "sign in to continue"
	if !errors.Is(err, auth.ErrNoCredentials) {
		// A credential that was present and rejected is worth logging: it is
		// either a bug in the sign-in flow or somebody trying things.
		s.logger.Warn("rejected a request",
			"path", r.URL.Path, "remote", r.RemoteAddr, "error", err)
		message = "your session is not valid; sign in again"
	}
	writeStatus(w, status, message)
}

// user returns the user a request belongs to.
//
// It returns nil only on a path that requireUser lets through, and those
// handlers do not read it. A handler that needs a user and finds none is a
// routing mistake, so callers use userID, which says so plainly.
func user(r *http.Request) *domain.User {
	found, _ := r.Context().Value(userKey{}).(*domain.User)
	return found
}

// userID returns the id of the user a request belongs to, or an empty string.
func (s *Server) userID(r *http.Request) string {
	if found := user(r); found != nil {
		return found.ID
	}
	// Only reachable if a handler reading a user is registered on an open
	// path. Returning empty scopes the query to nothing, which shows no data
	// rather than everyone's.
	s.logger.Error("a handler required a user but the request had none", "path", r.URL.Path)
	return ""
}

// authConfig tells the browser how to sign in. It is public by necessity and
// carries only publishable values.
func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	if s.authn == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"provider": "none", "sign_in_required": false,
		})
		return
	}
	config := s.authn.PublicConfig()

	// Whether this request is already signed in, so the page knows whether to
	// show a prompt without a second round trip.
	config["signed_in"] = false
	if identity, err := s.authn.Identify(r.Context(), r); err == nil && identity != nil {
		config["signed_in"] = true
		if identity.Email != "" {
			config["email"] = identity.Email
		}
	}
	writeJSON(w, http.StatusOK, config)
}

// whoami reports the signed-in user. The UI uses it for attribution display.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	current := user(r)
	if current == nil {
		writeStatus(w, http.StatusUnauthorized, "sign in to continue")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": current, "provider": s.authn.Name()})
}
