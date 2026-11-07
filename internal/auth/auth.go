// Package auth decides who is making a request.
//
// The control plane has no authentication of its own, which was tolerable only
// while it ran on a loopback address. Reached over the internet it hands out a
// shell, a secrets vault and the whole audit trail, so a boundary has to exist
// inside the process: anything in front of it -- a CDN access policy, a proxy
// password -- protects only the path that happens to go through it, and a
// request that reaches the origin with the right Host header bypasses it
// entirely.
//
// The identity provider is deliberately behind an interface. Who issues
// credentials is the part most likely to change; "which user is this request,
// and is it allowed" is the part the rest of the system is written against.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrNoCredentials means the request carried nothing to identify it. It is
// distinguished from a bad credential so a browser can be sent to sign in
// rather than told it failed.
var ErrNoCredentials = errors.New("auth: the request carried no credentials")

// ErrUnauthenticated means a credential was present and not acceptable.
var ErrUnauthenticated = errors.New("auth: the credential was not accepted")

// Identity is who a request belongs to, as the provider sees them. It is
// mapped onto a dabberz user separately: the provider owns identity, the
// store owns what that user has done.
type Identity struct {
	// Subject is the provider's stable identifier for this person. It is the
	// key the dabberz user is looked up by, because an email address can be
	// changed and reassigned while this cannot.
	Subject string
	// Email is used for display and attribution. It may be empty: not every
	// provider puts it in a token, and an identity is still usable without it.
	Email string
	// SessionID identifies the provider's session, for revocation and for the
	// audit trail. May be empty.
	SessionID string
}

// Authenticator identifies the person behind a request.
//
// Identify must not consider the request's path: what is readable by whom is
// authorization, decided by the caller. Mixing the two here is how an endpoint
// added later ends up unprotected by default.
type Authenticator interface {
	// Name identifies the authenticator in logs and in the UI.
	Name() string
	// Identify returns who the request belongs to, ErrNoCredentials if it
	// carried none, or ErrUnauthenticated if what it carried was rejected.
	Identify(ctx context.Context, r *http.Request) (*Identity, error)
	// PublicConfig is handed to the browser so it can present a sign-in flow.
	// It must contain nothing secret.
	PublicConfig() map[string]any
}

// SessionCookie is the cookie a credential is read from when there is no
// Authorization header.
//
// Cookies are not a fallback here, they are the primary channel. A browser
// cannot set headers on an EventSource or a WebSocket handshake, and the
// activity feed is server-sent events while the workspace shell is a
// WebSocket. Header-only authentication would leave exactly those two
// endpoints unprotected or broken.
const SessionCookie = "__session"

// BearerToken extracts a token from the Authorization header or the session
// cookie, in that order. It returns an empty string when neither is present.
func BearerToken(r *http.Request) string {
	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := cutBearer(header); ok {
			return token
		}
	}
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

// cutBearer accepts "Bearer <token>" case-insensitively on the scheme.
func cutBearer(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	return token, token != ""
}

// Single is an Authenticator that treats every request as one fixed person.
//
// It exists so the daemon runs on a developer's loopback address without an
// identity provider, and nowhere else: it accepts anything, so a deployment
// using it is open to whoever can reach the port. The daemon says so loudly at
// startup rather than letting it pass for configuration.
type Single struct {
	// Subject and Email describe the one person every request becomes.
	Subject string
	Email   string
}

// Name implements Authenticator.
func (Single) Name() string { return "single-user" }

// Identify implements Authenticator by returning the same person every time.
func (s Single) Identify(context.Context, *http.Request) (*Identity, error) {
	return &Identity{Subject: s.Subject, Email: s.Email}, nil
}

// PublicConfig implements Authenticator. There is nothing to sign in to.
func (Single) PublicConfig() map[string]any {
	return map[string]any{"provider": "single-user", "sign_in_required": false}
}

var _ Authenticator = Single{}
