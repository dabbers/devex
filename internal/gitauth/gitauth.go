// Package gitauth resolves the git credential a user's own work should be
// done with.
//
// A single shared deploy token does not survive more than one person. Whose
// repository is cloned, what the agent may push to, and what shows up in the
// commit history are all per-account questions, so the credential has to be
// per-account too.
//
// The token is not stored here. The identity provider already holds the user's
// GitHub connection, so it is asked for the token when one is needed and the
// answer is held briefly in memory. Nothing is written to the database, and
// nothing is sealed in the vault: a credential that is never stored cannot be
// leaked from storage, and revoking the connection at GitHub takes effect
// without dabberz having to notice.
package gitauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Provider names the forge a credential is for. Only GitHub is wired up; the
// field exists so the API and the UI do not have to be rewritten to add
// another.
const Provider = "github"

// Errors a caller can act on.
var (
	// ErrNotConfigured means this deployment cannot resolve tokens at all,
	// because the identity provider's backend credential is not set.
	ErrNotConfigured = errors.New("gitauth: no identity provider credential is configured")
	// ErrNotConnected means the user has not linked a GitHub account.
	ErrNotConnected = errors.New("gitauth: this account has no GitHub connection")
	// ErrSharedCredentials means the connection exists but the identity
	// provider will not hand over its token, because the provider is using the
	// shared development OAuth app rather than one of this deployment's own.
	ErrSharedCredentials = errors.New("gitauth: the identity provider is using shared development OAuth credentials, which cannot hand over an access token")
)

// Clerk error codes worth distinguishing. Both mean the same thing to a user
// -- connect a real OAuth app -- but the second also means they have to
// reconnect their account afterwards.
const (
	codeSharedNotSupported = "oauth_shared_credentials_token_retrieval_not_supported"
	codeSharedStored       = "oauth_shared_credentials_stored_token_not_retrievable"
)

// Users maps a dabberz user id onto the identity provider's subject.
//
// An interface rather than the store, because resolving a credential should
// not be able to read anything else.
type Users interface {
	ExternalID(ctx context.Context, userID string) (string, error)
}

// Link is what the UI shows about an account's GitHub connection.
type Link struct {
	// Configured reports whether this deployment can resolve tokens at all.
	Configured bool `json:"configured"`
	// Connected reports whether the user has linked a GitHub account.
	Connected bool `json:"connected"`
	// Account is the GitHub username, when the provider reports one.
	Account string `json:"account,omitempty"`
	// Usable reports whether a token could actually be obtained. A connection
	// can exist and still be unusable, which is the case worth naming.
	Usable bool `json:"usable"`
	// Scopes are the permissions the token carries.
	Scopes []string `json:"scopes,omitempty"`
	// Blocked explains, in a sentence a person can act on, why a connected
	// account is not usable.
	Blocked string `json:"blocked,omitempty"`
}

// tokenTTL bounds how long a resolved token is reused.
//
// Short, because the provider may be refreshing an expiring token behind this
// call and a stale one fails a clone minutes later. Long enough that one
// workspace start, which runs several git commands, costs one request.
const tokenTTL = 5 * time.Minute

// Resolver resolves git credentials from a Clerk instance.
type Resolver struct {
	secretKey string
	users     Users
	client    *http.Client
	endpoint  string
	now       func() time.Time

	mu     sync.Mutex
	cached map[string]cachedToken
}

type cachedToken struct {
	token   string
	scopes  []string
	expires time.Time
}

// Options configures a resolver.
type Options struct {
	// SecretKey is the identity provider's backend API key. Without it no
	// token can be resolved and every lookup reports that plainly.
	SecretKey string
	// Users resolves a dabberz user id to the provider's subject.
	Users Users
	// Endpoint overrides the provider's API base, for tests.
	Endpoint string
	// Client overrides the HTTP client.
	Client *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// New returns a resolver. It is usable with no secret key: every lookup then
// reports ErrNotConfigured, which the UI turns into an explanation rather than
// a failure.
func New(opts Options) *Resolver {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := strings.TrimSuffix(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://api.clerk.com/v1"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Resolver{
		secretKey: strings.TrimSpace(opts.SecretKey),
		users:     opts.Users,
		client:    client,
		endpoint:  endpoint,
		now:       now,
		cached:    make(map[string]cachedToken),
	}
}

// Configured reports whether this deployment can resolve tokens at all.
func (r *Resolver) Configured() bool { return r != nil && r.secretKey != "" && r.users != nil }

// Token returns the user's GitHub access token.
func (r *Resolver) Token(ctx context.Context, userID string) (string, error) {
	if !r.Configured() {
		return "", ErrNotConfigured
	}
	if token, ok := r.fromCache(userID); ok {
		return token, nil
	}

	subject, err := r.users.ExternalID(ctx, userID)
	if err != nil {
		return "", err
	}
	if subject == "" {
		// A single-user deployment has no provider subject, so there is no
		// connection to read.
		return "", ErrNotConnected
	}

	token, scopes, err := r.fetchToken(ctx, subject)
	if err != nil {
		return "", err
	}
	r.store(userID, token, scopes)
	return token, nil
}

// Status describes a user's connection, for the UI.
func (r *Resolver) Status(ctx context.Context, userID string) Link {
	if !r.Configured() {
		return Link{Blocked: "This deployment has no identity provider credential, so it cannot read your GitHub connection."}
	}

	link := Link{Configured: true}
	subject, err := r.users.ExternalID(ctx, userID)
	if err != nil || subject == "" {
		link.Blocked = "This account has no identity provider subject, so there is nothing to connect GitHub to."
		return link
	}

	if account, found, err := r.externalAccount(ctx, subject); err == nil {
		link.Connected, link.Account = found, account
	}

	token, scopes, err := r.fetchToken(ctx, subject)
	switch {
	case err == nil && token != "":
		link.Connected, link.Usable, link.Scopes = true, true, scopes
	case errors.Is(err, ErrNotConnected):
		link.Blocked = "Connect a GitHub account to clone and push with your own credentials."
	case errors.Is(err, ErrSharedCredentials):
		link.Connected = true
		link.Blocked = "GitHub is connected, but the identity provider is using its shared development OAuth app, " +
			"which will not hand over an access token. Register your own GitHub OAuth app, switch the connection " +
			"to custom credentials, then disconnect and reconnect GitHub here."
	case err != nil:
		link.Blocked = "GitHub is connected but the token could not be read: " + err.Error()
	}
	return link
}

// Forget drops a cached token, so a user who has just reconnected is not told
// about a credential that is no longer theirs.
func (r *Resolver) Forget(userID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cached, userID)
}

func (r *Resolver) fromCache(userID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cached[userID]
	if !ok || !r.now().Before(entry.expires) {
		return "", false
	}
	return entry.token, true
}

func (r *Resolver) store(userID, token string, scopes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached[userID] = cachedToken{token: token, scopes: scopes, expires: r.now().Add(tokenTTL)}
}

// fetchToken asks the provider for the user's GitHub token.
func (r *Resolver) fetchToken(ctx context.Context, subject string) (string, []string, error) {
	body, err := r.get(ctx, "/users/"+subject+"/oauth_access_tokens/"+Provider)
	if err != nil {
		return "", nil, err
	}

	tokens, err := decodeTokens(body)
	if err != nil {
		return "", nil, err
	}
	for _, token := range tokens {
		if strings.TrimSpace(token.Token) != "" {
			return token.Token, token.Scopes, nil
		}
	}
	return "", nil, ErrNotConnected
}

// oauthToken is one provider token as the backend API reports it.
type oauthToken struct {
	Token    string   `json:"token"`
	Provider string   `json:"provider"`
	Scopes   []string `json:"scopes"`
	Label    string   `json:"label"`
}

// decodeTokens reads the token list, which the API has spelled both as a bare
// array and as a paginated object. Accepting both means a change at the
// provider does not silently stop every clone.
func decodeTokens(body []byte) ([]oauthToken, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var tokens []oauthToken
		if err := json.Unmarshal(body, &tokens); err != nil {
			return nil, fmt.Errorf("gitauth: unreadable token list: %w", err)
		}
		return tokens, nil
	}

	var page struct {
		Data []oauthToken `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("gitauth: unreadable token list: %w", err)
	}
	return page.Data, nil
}

// externalAccount reports whether the user has a GitHub account linked, and
// under what name.
//
// This is a separate question from whether a token can be read: a connection
// the provider will not hand a token for still exists, and saying "not
// connected" would send the user to reconnect something that is already there.
func (r *Resolver) externalAccount(ctx context.Context, subject string) (string, bool, error) {
	body, err := r.get(ctx, "/users/"+subject)
	if err != nil {
		return "", false, err
	}

	var user struct {
		ExternalAccounts []struct {
			Provider string `json:"provider"`
			Username string `json:"username"`
		} `json:"external_accounts"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return "", false, fmt.Errorf("gitauth: unreadable user: %w", err)
	}
	for _, account := range user.ExternalAccounts {
		// The provider is spelled "oauth_github" on the user object and
		// "github" on a token, so both are accepted.
		if account.Provider == Provider || account.Provider == "oauth_"+Provider {
			return account.Username, true, nil
		}
	}
	return "", false, nil
}

// get calls the provider's backend API.
func (r *Resolver) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.secretKey)
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitauth: reach the identity provider: %w", err)
	}
	defer resp.Body.Close()

	// Bounded: this is a token list, and an unbounded read of a response from
	// anywhere is a way to be made to allocate.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gitauth: read the identity provider's answer: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return body, nil
	}
	return nil, providerError(resp.StatusCode, body)
}

// providerError turns the provider's error body into something a caller can
// branch on.
func providerError(status int, body []byte) error {
	var failure struct {
		Errors []struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			LongMessage string `json:"long_message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &failure)

	for _, reported := range failure.Errors {
		switch reported.Code {
		case codeSharedNotSupported, codeSharedStored:
			return ErrSharedCredentials
		}
	}
	if status == http.StatusNotFound {
		return ErrNotConnected
	}

	detail := strings.TrimSpace(string(body))
	if len(failure.Errors) > 0 {
		detail = failure.Errors[0].Message
		if failure.Errors[0].LongMessage != "" {
			detail = failure.Errors[0].LongMessage
		}
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return fmt.Errorf("gitauth: the identity provider answered %d: %s", status, detail)
}
