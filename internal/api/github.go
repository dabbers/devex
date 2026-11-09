package api

import (
	"context"
	"net/http"

	"github.com/dabbers/devex/internal/gitauth"
)

// GitLinks is what the API needs from credential resolution: the status of an
// account's connection, and a way to forget what was remembered about it.
type GitLinks interface {
	Status(ctx context.Context, userID string) gitauth.Link
	Forget(userID string)
}

// The account's GitHub connection.
//
// This is a status surface, not a credential one: it says whether the signed-in
// account can clone and push as themselves, and when it cannot, what to do
// about it. No token is returned, logged or recorded. The connection itself is
// made and broken in the identity provider's own account UI, which is what
// makes it self-serve -- dabberz never sees the OAuth exchange.

func (s *Server) githubLink(w http.ResponseWriter, r *http.Request) {
	if s.gitTokens == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"github": map[string]any{
				"configured": false,
				"blocked":    "This deployment is not configured to link GitHub accounts.",
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"github": s.gitTokens.Status(r.Context(), s.userID(r)),
	})
}

// recheckGithubLink drops what is remembered about the account's credential
// and looks again.
//
// A resolved token is held briefly in memory, which is what keeps one
// workspace start from making a request per git command. That cache is also
// why someone who has just reconnected their account would otherwise be told
// for several minutes about a connection they have already replaced.
func (s *Server) recheckGithubLink(w http.ResponseWriter, r *http.Request) {
	if s.gitTokens == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment is not configured to link GitHub accounts")
		return
	}
	s.gitTokens.Forget(s.userID(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"github": s.gitTokens.Status(r.Context(), s.userID(r)),
	})
}
