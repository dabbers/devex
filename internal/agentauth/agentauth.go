// Package agentauth resolves the subscription credential an agent run is
// driven by.
//
// Customers bring their own subscription -- Claude, Codex or Grok -- and the
// platform never pays for an agent run with a key of its own. So the
// credential a run authenticates with is a per-account question, answered
// from the one credential that account stored, the same way internal/gitauth
// answers whose GitHub connection a clone uses. A run in one customer's
// workspace gets that customer's credential, and a customer with none gets a
// refusal that says so, never somebody else's.
//
// The stored credential is tagged with its provider, one active per account,
// so adding a provider is a new runner rather than a new schema. Only Claude
// has a runner today; the others are recognised, can be stored, and are
// refused at run time with a message that says so.
//
// The daemon's own CLAUDE_CODE_OAUTH_TOKEN survives only as an explicit,
// single-owner fallback: when turned on, it is used for the configured owner's
// runs and for nobody else's. A process-wide token handed to any account that
// lacks one would be the old behaviour with an extra step.
package agentauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/secrets"
)

// Kind is the vault's name for the agent credential.
const Kind = "agent"

// maxCredentialLen bounds what is accepted as a credential. Real ones are a
// few hundred characters at most; the bound is there so the endpoint is not a
// way to store arbitrary blobs in the vault.
const maxCredentialLen = 4096

// Provider describes one subscription the agent can be driven by.
type Provider struct {
	ID   domain.AgentProvider `json:"id"`
	Name string               `json:"name"`
	// Supported reports whether this deployment has a runner for it. A
	// provider that is not supported can be stored, so a customer can set up
	// ahead of time, but its runs are refused.
	Supported bool `json:"supported"`
	// Obtain tells a person how to get the credential to paste.
	Obtain string `json:"obtain"`
}

// Providers is every provider dabberz recognises, in the order a panel should
// offer them.
var Providers = []Provider{
	{
		ID: domain.ProviderClaude, Name: "Claude", Supported: true,
		Obtain: "Run `claude setup-token` with Claude Code signed in to your Claude subscription, and paste the token it prints.",
	},
	{
		ID: domain.ProviderCodex, Name: "Codex", Supported: false,
		Obtain: "Not supported yet: dabberz cannot drive Codex with your ChatGPT subscription yet.",
	},
	{
		ID: domain.ProviderGrok, Name: "Grok", Supported: false,
		Obtain: "Not supported yet: dabberz cannot drive Grok with your subscription yet.",
	},
}

// Lookup returns a recognised provider.
func Lookup(id domain.AgentProvider) (Provider, bool) {
	for _, provider := range Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return Provider{}, false
}

// ErrNoCredential means the account has stored no agent credential and no
// fallback applies to it. The message is the one a customer sees, so it says
// what to do.
var ErrNoCredential = errors.New("this account has no agent subscription credential, so the agent cannot run; " +
	"add your Claude token under Account (PUT /v1/account/agent)")

// Credential is what a run authenticates with.
type Credential struct {
	Provider domain.AgentProvider
	// Secret is never serialised: nothing that marshals a Credential should
	// be able to put the value on the wire by accident.
	Secret string `json:"-"`
}

// Source says where a resolved credential came from.
type Source string

const (
	// SourceNone means no credential would be used: runs are refused.
	SourceNone Source = "none"
	// SourceAccount is the account's own stored credential.
	SourceAccount Source = "account"
	// SourceOwnerFallback is the daemon's Claude token, used because this
	// account is the configured owner and the fallback is on.
	SourceOwnerFallback Source = "owner_fallback"
)

// Vault is what resolution needs from the secrets vault: opening one
// credential, and seeing whether one exists without opening it.
type Vault interface {
	Credential(ctx context.Context, userID, kind string) (provider, value string, err error)
	CredentialSet(ctx context.Context, userID, kind string) (*secrets.CredentialInfo, error)
}

// Options configures a resolver.
type Options struct {
	// Vault holds each account's sealed credential. Without it no account
	// has one, and only the owner fallback can resolve anything.
	Vault Vault
	// FallbackToken is the daemon's own Claude token. It is consulted only
	// for FallbackUserID, and only when that account has stored nothing.
	FallbackToken string
	// FallbackUserID is the single account the fallback applies to. Leaving
	// it empty turns the fallback off, whatever FallbackToken holds.
	FallbackUserID string
}

// Resolver resolves agent credentials per account.
type Resolver struct {
	vault          Vault
	fallbackToken  string
	fallbackUserID string
}

// New returns a resolver.
//
// A fallback account with no fallback token is refused rather than quietly
// treated as off: someone asked for the owner's runs to keep working, and
// starting up as though they had not is the failure that only shows on the
// first change somebody asks for.
func New(opts Options) (*Resolver, error) {
	token := strings.TrimSpace(opts.FallbackToken)
	if opts.FallbackUserID != "" && token == "" {
		return nil, errors.New("agentauth: the owner fallback is on but the daemon has no Claude token to fall back to")
	}
	r := &Resolver{vault: opts.Vault}
	if opts.FallbackUserID != "" {
		r.fallbackToken, r.fallbackUserID = token, opts.FallbackUserID
	}
	return r, nil
}

// AgentCredential returns what a run for userID authenticates with.
//
// It never returns another account's credential. An account with none gets
// ErrNoCredential unless it is the configured owner and the fallback is on.
// Whether the provider can actually be run is the runner's question: the
// credential is returned as stored, tagged with its provider.
//
// A run naming no account is refused by the agent runner before it gets
// here; if one did arrive it would find nothing stored, and the fallback
// never matches an empty id, so it is refused here too.
func (r *Resolver) AgentCredential(ctx context.Context, userID string) (Credential, error) {
	if r.vault != nil {
		provider, secret, err := r.vault.Credential(ctx, userID, Kind)
		switch {
		case err == nil && secret != "":
			return Credential{Provider: domain.AgentProvider(provider), Secret: secret}, nil
		case err != nil && !errors.Is(err, secrets.ErrNotFound):
			// A credential that is stored but cannot be opened is not the
			// same as none. Falling back here would spend the owner's
			// subscription on a failure the customer ought to hear about.
			return Credential{}, fmt.Errorf("agentauth: read this account's agent credential: %w", err)
		}
	}
	if r.fallbackUserID != "" && userID == r.fallbackUserID {
		return Credential{Provider: domain.ProviderClaude, Secret: r.fallbackToken}, nil
	}
	return Credential{}, ErrNoCredential
}

// Status is what the panel shows about an account's agent credential. It
// never carries the value.
type Status struct {
	// Provider is what runs would be driven by right now; empty when none.
	Provider domain.AgentProvider `json:"provider"`
	// Set reports whether the account has stored a credential of its own.
	Set bool `json:"set"`
	// UpdatedAt is when it was last stored.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Source is where the credential runs would use comes from.
	Source Source `json:"source"`
	// Supported reports whether Provider can actually be run here.
	Supported bool `json:"supported"`
	// Blocked explains, in a sentence a person can act on, why runs would be
	// refused. Empty when they would not be.
	Blocked string `json:"blocked,omitempty"`
}

// Status describes an account's credential without opening it.
func (r *Resolver) Status(ctx context.Context, userID string) (Status, error) {
	var status Status
	if r.vault != nil && userID != "" {
		info, err := r.vault.CredentialSet(ctx, userID, Kind)
		if err != nil {
			return Status{}, err
		}
		if info != nil {
			updated := info.UpdatedAt
			status.Set, status.UpdatedAt = true, &updated
			status.Provider, status.Source = domain.AgentProvider(info.Provider), SourceAccount
		}
	}
	if !status.Set {
		if r.fallbackUserID != "" && userID == r.fallbackUserID {
			status.Provider, status.Source = domain.ProviderClaude, SourceOwnerFallback
		} else {
			status.Source = SourceNone
			status.Blocked = ErrNoCredential.Error()
			return status, nil
		}
	}
	provider, known := Lookup(status.Provider)
	status.Supported = known && provider.Supported
	if !status.Supported {
		status.Blocked = Unsupported(status.Provider).Error()
	}
	return status, nil
}

// Unsupported is the refusal for a provider this deployment cannot run.
func Unsupported(id domain.AgentProvider) error {
	name := string(id)
	if provider, known := Lookup(id); known {
		name = provider.Name
	}
	return fmt.Errorf("the agent cannot be driven by %s yet; only Claude is supported for now, "+
		"so add a Claude token under Account (PUT /v1/account/agent) to run it", name)
}

// Validate checks a credential a customer is storing. It says what is wrong
// without repeating the value, which is the one thing an error about a
// credential must never do.
func Validate(id domain.AgentProvider, secret string) error {
	if _, known := Lookup(id); !known {
		return fmt.Errorf("agentauth: %q is not a provider dabberz recognises; use claude, codex or grok", string(id))
	}
	if secret == "" {
		return errors.New("agentauth: the credential is empty")
	}
	if len(secret) > maxCredentialLen {
		return fmt.Errorf("agentauth: the credential is longer than %d characters; that is not a subscription token", maxCredentialLen)
	}
	for _, c := range secret {
		// It travels as an environment variable. Whitespace is almost always a
		// paste that took the newline with it, and a control character has no
		// business in a credential.
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return errors.New("agentauth: the credential contains whitespace or control characters; paste it again without them")
		}
	}
	return nil
}
