package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ClerkConfig configures authentication against Clerk.
//
// Only the publishable key is required, and it is not a secret: it encodes the
// instance's frontend host, which is all that is needed to derive the issuer
// and the published key set. Verifying a session token needs no secret at all,
// which is worth stating because it is easy to assume otherwise and hand a
// secret to something that does not need one.
type ClerkConfig struct {
	// PublishableKey looks like pk_test_<base64 of "host$">. It is sent to the
	// browser, so it must not be treated as a credential.
	PublishableKey string `yaml:"publishable_key" json:"publishable_key"`
	// SecretKey is used only for Clerk's backend API, which is consulted to
	// learn a new user's email address when the session token does not carry
	// one. Authentication works without it; attribution is just poorer.
	SecretKey string `yaml:"secret_key" json:"-"`
	// AuthorizedParties restricts which origins' tokens are accepted. Leave it
	// empty only when the deployment's public origin is not known in advance.
	AuthorizedParties []string `yaml:"authorized_parties" json:"authorized_parties"`
}

// Validate checks the configuration.
func (c ClerkConfig) Validate() error {
	if strings.TrimSpace(c.PublishableKey) == "" {
		return errors.New("auth: clerk needs a publishable key")
	}
	if _, err := clerkHost(c.PublishableKey); err != nil {
		return err
	}
	return nil
}

// clerkHost decodes the frontend host out of a publishable key.
//
// The key is "pk_live_" or "pk_test_" followed by unpadded base64 of the host
// with a trailing "$". Deriving the host rather than configuring it separately
// removes a way for the two to disagree.
func clerkHost(publishableKey string) (string, error) {
	key := strings.TrimSpace(publishableKey)
	var body string
	switch {
	case strings.HasPrefix(key, "pk_test_"):
		body = strings.TrimPrefix(key, "pk_test_")
	case strings.HasPrefix(key, "pk_live_"):
		body = strings.TrimPrefix(key, "pk_live_")
	default:
		return "", fmt.Errorf("auth: publishable key does not start with pk_test_ or pk_live_")
	}

	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(body, "="))
	if err != nil {
		return "", fmt.Errorf("auth: publishable key is not base64: %w", err)
	}
	host := strings.TrimSuffix(string(decoded), "$")
	if host == "" || strings.ContainsAny(host, "/ :") {
		return "", fmt.Errorf("auth: publishable key decodes to %q, which is not a hostname", host)
	}
	return host, nil
}

// Clerk identifies requests by verifying Clerk session tokens.
type Clerk struct {
	cfg       ClerkConfig
	issuer    string
	keys      *keySet
	client    *http.Client
	now       func() time.Time
	emailOnce emailCache
}

// NewClerk returns an authenticator for the configured Clerk instance.
func NewClerk(cfg ClerkConfig) (*Clerk, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	host, err := clerkHost(cfg.PublishableKey)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: keySetFetchTimeout}
	return &Clerk{
		cfg:       cfg,
		issuer:    "https://" + host,
		keys:      newKeySet("https://"+host+"/.well-known/jwks.json", client),
		client:    client,
		now:       time.Now,
		emailOnce: emailCache{entries: map[string]string{}},
	}, nil
}

// Name implements Authenticator.
func (c *Clerk) Name() string { return "clerk" }

// Issuer is the expected token issuer, exposed for diagnostics.
func (c *Clerk) Issuer() string { return c.issuer }

// Identify implements Authenticator.
func (c *Clerk) Identify(ctx context.Context, r *http.Request) (*Identity, error) {
	token := BearerToken(r)
	if token == "" {
		return nil, ErrNoCredentials
	}

	identity, err := verifyToken(token, c.keys, verifyOptions{
		Issuer:            c.issuer,
		AuthorizedParties: c.cfg.AuthorizedParties,
	}, c.now())
	if err != nil {
		return nil, err
	}

	// A Clerk session token carries no email unless the instance is configured
	// to add one, and an audit trail that attributes work to "user_2abc..."
	// instead of a person is materially worse. So it is looked up once per
	// subject, and a failure is not fatal: the identity is still usable.
	if identity.Email == "" && c.cfg.SecretKey != "" {
		if email, ok := c.emailOnce.get(identity.Subject); ok {
			identity.Email = email
		} else if email, err := c.fetchEmail(ctx, identity.Subject); err == nil {
			c.emailOnce.put(identity.Subject, email)
			identity.Email = email
		}
	}
	return identity, nil
}

// PublicConfig implements Authenticator, handing the browser what it needs to
// run a sign-in flow. The publishable key is designed to be public.
func (c *Clerk) PublicConfig() map[string]any {
	return map[string]any{
		"provider":         "clerk",
		"sign_in_required": true,
		"publishable_key":  c.cfg.PublishableKey,
		"frontend_api":     strings.TrimPrefix(c.issuer, "https://"),
	}
}

// clerkUser is the part of Clerk's user representation that is needed.
type clerkUser struct {
	PrimaryEmailAddressID string `json:"primary_email_address_id"`
	EmailAddresses        []struct {
		ID           string `json:"id"`
		EmailAddress string `json:"email_address"`
	} `json:"email_addresses"`
}

// fetchEmail asks Clerk's backend API for a subject's primary email address.
func (c *Clerk) fetchEmail(ctx context.Context, subject string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, keySetFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.clerk.com/v1/users/"+subject, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.SecretKey)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clerk user lookup returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var user clerkUser
	if err := json.Unmarshal(body, &user); err != nil {
		return "", err
	}
	for _, address := range user.EmailAddresses {
		if address.ID == user.PrimaryEmailAddressID && address.EmailAddress != "" {
			return address.EmailAddress, nil
		}
	}
	if len(user.EmailAddresses) > 0 {
		return user.EmailAddresses[0].EmailAddress, nil
	}
	return "", fmt.Errorf("clerk user %s has no email address", subject)
}

// emailCache remembers resolved email addresses so the backend API is called
// once per person rather than once per request.
type emailCache struct {
	mu      sync.Mutex
	entries map[string]string
}

func (e *emailCache) get(subject string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	email, ok := e.entries[subject]
	return email, ok
}

func (e *emailCache) put(subject, email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.entries == nil {
		e.entries = map[string]string{}
	}
	e.entries[subject] = email
}

var _ Authenticator = (*Clerk)(nil)
