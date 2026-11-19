package claudeauth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
)

type accounts struct {
	ctx   context.Context
	db    *store.Store
	vault *secrets.Vault
	a, b  string
}

// twoAccounts is two customers in one real vault, so resolution is tested
// against the storage it will actually read rather than a map.
func twoAccounts(t *testing.T) accounts {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	vault, err := secrets.NewVault(db, make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	a, err := db.EnsureUser(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	b, err := db.EnsureUser(ctx, "b@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	return accounts{ctx: ctx, db: db, vault: vault, a: a.ID, b: b.ID}
}

func resolver(t *testing.T, opts Options) *Resolver {
	t.Helper()
	r, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestEachAccountResolvesToItsOwnTokenAndNoOther is the resolver's half of
// the guarantee: A gets A's, B gets B's, an account with none gets a refusal,
// and the daemon's token reaches nobody unless the fallback names them.
func TestEachAccountResolvesToItsOwnTokenAndNoOther(t *testing.T) {
	acc := twoAccounts(t)
	if err := acc.vault.SetCredential(acc.ctx, acc.a, Kind, "tok-a"); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	if err := acc.vault.SetCredential(acc.ctx, acc.b, Kind, "tok-b"); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	// The fallback is configured, but for A: B must never see it, and A's own
	// stored token wins over it.
	r := resolver(t, Options{Vault: acc.vault, FallbackToken: "tok-daemon", FallbackUserID: acc.a})

	for user, want := range map[string]string{acc.a: "tok-a", acc.b: "tok-b"} {
		got, err := r.ClaudeToken(acc.ctx, user)
		if err != nil {
			t.Fatalf("ClaudeToken(%s): %v", user, err)
		}
		if got != want {
			t.Fatalf("ClaudeToken(%s) = %q, want %q", user, got, want)
		}
	}

	// B removes theirs. B is not the owner, so B is refused -- not handed
	// A's token, and not the daemon's.
	if err := acc.vault.DeleteCredential(acc.ctx, acc.b, Kind); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	got, err := r.ClaudeToken(acc.ctx, acc.b)
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("an account with no token resolved to %q (err %v), want ErrNoToken", got, err)
	}
	if got != "" {
		t.Fatalf("a refusal came with a token: %q", got)
	}
}

func TestTheFallbackIsOffByDefaultAndOnlyEverForTheOwner(t *testing.T) {
	acc := twoAccounts(t)

	// Off: a daemon token without a named owner is never used, by anyone.
	off := resolver(t, Options{Vault: acc.vault, FallbackToken: "tok-daemon"})
	for _, user := range []string{acc.a, acc.b} {
		if _, err := off.ClaudeToken(acc.ctx, user); !errors.Is(err, ErrNoToken) {
			t.Fatalf("with the fallback off, %s was not refused: %v", user, err)
		}
	}

	// On, for A: A's runs use it until A stores their own; B's never do.
	on := resolver(t, Options{Vault: acc.vault, FallbackToken: "tok-daemon", FallbackUserID: acc.a})
	if got, err := on.ClaudeToken(acc.ctx, acc.a); err != nil || got != "tok-daemon" {
		t.Fatalf("the owner's fallback = %q, %v", got, err)
	}
	if got, err := on.ClaudeToken(acc.ctx, acc.b); !errors.Is(err, ErrNoToken) {
		t.Fatalf("another account received the owner's fallback: %q, %v", got, err)
	}
	if _, err := on.ClaudeToken(acc.ctx, ""); err == nil {
		t.Fatal("a run naming no account resolved a token")
	}

	// Asking for a fallback with nothing to fall back to is refused at once.
	if _, err := New(Options{Vault: acc.vault, FallbackUserID: acc.a}); err == nil {
		t.Fatal("a fallback owner with no fallback token was accepted")
	}
}

func TestAStoredTokenThatCannotBeOpenedIsAnErrorNotAFallback(t *testing.T) {
	acc := twoAccounts(t)
	if err := acc.vault.SetCredential(acc.ctx, acc.a, Kind, "tok-a"); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	// The same rows read with another master key: a rotated or lost key.
	rekeyed, err := secrets.NewVault(acc.db, []byte(strings.Repeat("k", secrets.KeySize)))
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	r := resolver(t, Options{Vault: rekeyed, FallbackToken: "tok-daemon", FallbackUserID: acc.a})
	got, err := r.ClaudeToken(acc.ctx, acc.a)
	if err == nil || errors.Is(err, ErrNoToken) || got != "" {
		t.Fatalf("an unreadable token resolved to %q (err %v), want a plain error", got, err)
	}
}

func TestStatusNeverOpensOrReturnsTheToken(t *testing.T) {
	acc := twoAccounts(t)
	r := resolver(t, Options{Vault: acc.vault, FallbackToken: "tok-daemon", FallbackUserID: acc.a})

	status, err := r.Status(acc.ctx, acc.b)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Set || status.Source != SourceNone || status.Blocked == "" {
		t.Fatalf("an account with nothing = %+v", status)
	}
	if status, _ := r.Status(acc.ctx, acc.a); status.Source != SourceOwnerFallback {
		t.Fatalf("the owner with no token of their own = %+v", status)
	}

	if err := acc.vault.SetCredential(acc.ctx, acc.b, Kind, "tok-b"); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	status, err = r.Status(acc.ctx, acc.b)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Set || status.Source != SourceAccount || status.UpdatedAt == nil {
		t.Fatalf("an account with its own token = %+v", status)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("sk-ant-oat01-abcdef"); err != nil {
		t.Fatalf("a plausible token was refused: %v", err)
	}
	for _, bad := range []string{"", "a b", "a\nb", "a\tb", "a\x07b", strings.Repeat("x", maxTokenLen+1)} {
		err := Validate(bad)
		if err == nil {
			t.Errorf("Validate(%q) accepted it", bad)
			continue
		}
		if len(bad) > 3 && strings.Contains(err.Error(), bad) {
			t.Errorf("the error repeats the value: %v", err)
		}
	}
}
