// Package identity translates a request's credential into the dabberz user the
// rest of the platform is written against.
//
// This exists because there are now two front doors. The control plane
// authenticates its own API, and the edge authenticates visitors to an
// installation so it can tell an app who is asking. Both have to answer the
// same question, and the answer has two steps that must not come apart: the
// provider says who this is, and the store says which dabberz user that is.
//
// Keeping the rule in one place is the whole point. If the edge translated
// differently from the panel -- a provider subject here, a user id there --
// then an app's idea of who owns something would not match the control
// plane's, and the symptom would not be an error. It would be a person
// looking at their own installation while the app treats them as a stranger.
package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/domain"
)

// ErrUnresolved means the credential was accepted but no dabberz user could be
// resolved from it. It is distinct from the auth errors because the caller
// says something different: the credential is fine, the account is not.
var ErrUnresolved = errors.New("identity: the account could not be resolved")

// Users is the store, as the translation needs it.
type Users interface {
	EnsureUserByExternalID(ctx context.Context, externalID, email string) (*domain.User, error)
}

// Translator answers "which dabberz user is this request".
type Translator struct {
	authn auth.Authenticator
	users Users
}

// New returns a translator. Both collaborators are required: without an
// authenticator there is no credential to read, and without a store there is
// nothing to translate it into.
func New(authn auth.Authenticator, users Users) (*Translator, error) {
	switch {
	case authn == nil:
		return nil, fmt.Errorf("identity: an authenticator is required")
	case users == nil:
		return nil, fmt.Errorf("identity: a store is required")
	}
	return &Translator{authn: authn, users: users}, nil
}

// Visitor returns the dabberz user behind a request.
//
// The auth errors are returned as they are, so a caller can still tell "no
// credential" from "a credential I rejected" -- the difference between sending
// a browser to sign in and telling it that it failed. Anything wrong with the
// account itself comes back as ErrUnresolved.
//
// First sign-in creates the user. That is true at the edge as well as in the
// panel, deliberately: the provider has already vouched for this person, so
// refusing to record them would mean a colleague invited to an app is a
// stranger to it until they happen to visit the panel first.
func (t *Translator) Visitor(ctx context.Context, r *http.Request) (*domain.User, error) {
	who, err := t.authn.Identify(ctx, r)
	if err != nil {
		return nil, err
	}
	if who == nil || who.Subject == "" {
		return nil, auth.ErrNoCredentials
	}

	user, err := t.users.EnsureUserByExternalID(ctx, who.Subject, who.Email)
	if err != nil {
		return nil, fmt.Errorf("%w: subject %s: %w", ErrUnresolved, who.Subject, err)
	}
	return user, nil
}
