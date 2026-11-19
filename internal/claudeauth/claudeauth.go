// Package claudeauth resolves the Claude credential an agent run is paid
// with.
//
// Customers bring their own Claude subscription; that is the pitch. So the
// token a run authenticates with is a per-account question, answered from the
// credential that account stored, the same way internal/gitauth answers whose
// GitHub connection a clone uses. A run in one customer's workspace gets that
// customer's token, and a customer with no token gets a refusal that says so,
// never somebody else's.
//
// The daemon's own CLAUDE_CODE_OAUTH_TOKEN survives only as an explicit,
// single-owner fallback: when turned on, it is used for the configured owner's
// runs and for nobody else's. A process-wide token handed to any account that
// lacks one would be the old behaviour with an extra step.
package claudeauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/dabbers/devex/internal/secrets"
)

// Kind is the vault's name for this credential.
const Kind = "claude_oauth_token"

// maxTokenLen bounds what is accepted as a token. Real ones are around a
// hundred characters; the bound is there so the endpoint is not a way to
// store arbitrary blobs in the vault.
const maxTokenLen = 4096

// ErrNoToken means the account has stored no Claude token and no fallback
// applies to it. The message is the one a customer sees, so it says what to do.
var ErrNoToken = errors.New("this account has no Claude token, so the agent cannot run; " +
	"generate one with `claude setup-token` and save it under Settings (PUT /v1/claude)")

// Source says where a resolved token came from.
type Source string

const (
	// SourceNone means no token would be used: runs are refused.
	SourceNone Source = "none"
	// SourceAccount is the account's own stored token.
	SourceAccount Source = "account"
	// SourceOwnerFallback is the daemon's token, used because this account
	// is the configured owner and the fallback is on.
	SourceOwnerFallback Source = "owner_fallback"
)

// Vault is what resolution needs from the secrets vault: opening one
// credential, and seeing whether one exists without opening it.
type Vault interface {
	Credential(ctx context.Context, userID, kind string) (string, error)
	CredentialSet(ctx context.Context, userID, kind string) (bool, time.Time, error)
}

// Options configures a resolver.
type Options struct {
	// Vault holds each account's sealed token. Without it no account has one,
	// and only the owner fallback can resolve anything.
	Vault Vault
	// FallbackToken is the daemon's own token. It is consulted only for
	// FallbackUserID, and only when that account has stored none.
	FallbackToken string
	// FallbackUserID is the single account the fallback applies to. Leaving
	// it empty turns the fallback off, whatever FallbackToken holds.
	FallbackUserID string
}

// Resolver resolves Claude tokens per account.
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
		return nil, errors.New("claudeauth: the owner fallback is on but the daemon has no Claude token to fall back to")
	}
	r := &Resolver{vault: opts.Vault}
	if opts.FallbackUserID != "" {
		r.fallbackToken, r.fallbackUserID = token, opts.FallbackUserID
	}
	return r, nil
}

// ClaudeToken returns the token a run for userID authenticates with.
//
// It never returns another account's token. An account with none gets
// ErrNoToken unless it is the configured owner and the fallback is on.
//
// A run naming no account is refused by the agent runner before it gets here;
// if one did arrive it would find no stored token, and the fallback never
// matches an empty id, so it is refused here too.
func (r *Resolver) ClaudeToken(ctx context.Context, userID string) (string, error) {
	if r.vault != nil {
		token, err := r.vault.Credential(ctx, userID, Kind)
		switch {
		case err == nil && strings.TrimSpace(token) != "":
			return token, nil
		case err != nil && !errors.Is(err, secrets.ErrNotFound):
			// A token that is stored but cannot be opened is not the same as
			// no token. Falling back here would spend the owner's
			// subscription on a failure the customer ought to hear about.
			return "", fmt.Errorf("claudeauth: read this account's Claude token: %w", err)
		}
	}
	if r.fallbackUserID != "" && userID == r.fallbackUserID {
		return r.fallbackToken, nil
	}
	return "", ErrNoToken
}

// Status is what the UI shows about an account's Claude token. It never
// carries the token.
type Status struct {
	// Set reports whether the account has stored a token of its own.
	Set bool `json:"set"`
	// UpdatedAt is when it was last stored.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Source is what an agent run would authenticate with right now.
	Source Source `json:"source"`
	// Blocked explains, in a sentence a person can act on, why runs would be
	// refused.
	Blocked string `json:"blocked,omitempty"`
}

// Status describes an account's token without opening it.
func (r *Resolver) Status(ctx context.Context, userID string) (Status, error) {
	var status Status
	if r.vault != nil && userID != "" {
		set, updated, err := r.vault.CredentialSet(ctx, userID, Kind)
		if err != nil {
			return Status{}, err
		}
		if set {
			status.Set, status.UpdatedAt = true, &updated
		}
	}
	switch {
	case status.Set:
		status.Source = SourceAccount
	case r.fallbackUserID != "" && userID == r.fallbackUserID:
		status.Source = SourceOwnerFallback
	default:
		status.Source = SourceNone
		status.Blocked = ErrNoToken.Error()
	}
	return status, nil
}

// Validate checks a token a customer is storing. It says what is wrong
// without repeating the value, which is the one thing an error about a
// credential must never do.
func Validate(token string) error {
	if token == "" {
		return errors.New("claudeauth: the token is empty")
	}
	if len(token) > maxTokenLen {
		return fmt.Errorf("claudeauth: the token is longer than %d characters; that is not a Claude token", maxTokenLen)
	}
	for _, c := range token {
		// It travels as an environment variable. Whitespace is almost always a
		// paste that took the newline with it, and a control character has no
		// business in a credential.
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return errors.New("claudeauth: the token contains whitespace or control characters; paste it again without them")
		}
	}
	return nil
}
