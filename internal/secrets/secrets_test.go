package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

func newVault(t *testing.T) (*Vault, *store.Store, *domain.Repo, context.Context) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	user, err := st.EnsureUser(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	repo := &domain.Repo{UserID: user.ID, Name: "devex", RemoteURL: "git@example.com:devex.git"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	encoded, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key, err := ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	v, err := NewVault(st, key)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	return v, st, repo, ctx
}

func TestKeyLifecycle(t *testing.T) {
	encoded, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key, err := ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("key is %d bytes, want %d", len(key), KeySize)
	}
	if _, err := ParseKey("not-hex"); err == nil {
		t.Error("ParseKey should reject non-hex input")
	}
	if _, err := ParseKey("abcd"); err == nil {
		t.Error("ParseKey should reject a short key")
	}
	// Two generated keys must differ.
	other, _ := GenerateKey()
	if other == encoded {
		t.Error("GenerateKey returned the same key twice")
	}
}

func TestNewVaultValidatesInputs(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	if _, err := NewVault(nil, make([]byte, KeySize)); err == nil {
		t.Error("NewVault should require a store")
	}
	if _, err := NewVault(st, make([]byte, 8)); err == nil {
		t.Error("NewVault should reject a short key")
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	v, _, repo, ctx := newVault(t)

	if err := v.Set(ctx, repo.ID, "DATABASE_URL", "postgres://localhost/app"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := v.Get(ctx, repo.ID, "DATABASE_URL")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "postgres://localhost/app" {
		t.Fatalf("Get = %q", got)
	}
}

func TestValuesAreNotStoredInPlaintext(t *testing.T) {
	v, st, repo, ctx := newVault(t)
	const value = "super-secret-token-value"
	if err := v.Set(ctx, repo.ID, "API_TOKEN", value); err != nil {
		t.Fatalf("Set: %v", err)
	}

	row, err := st.GetSecret(ctx, repo.ID, "API_TOKEN")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if strings.Contains(string(row.Ciphertext), value) {
		t.Fatal("the plaintext value is readable in the stored ciphertext")
	}
	if len(row.Nonce) == 0 {
		t.Fatal("no nonce was stored")
	}
}

func TestRotatingAValueUsesAFreshNonce(t *testing.T) {
	v, st, repo, ctx := newVault(t)
	if err := v.Set(ctx, repo.ID, "API_TOKEN", "same-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	first, _ := st.GetSecret(ctx, repo.ID, "API_TOKEN")
	firstNonce := string(first.Nonce)
	firstCipher := string(first.Ciphertext)

	if err := v.Set(ctx, repo.ID, "API_TOKEN", "same-value"); err != nil {
		t.Fatalf("Set (rotate): %v", err)
	}
	second, _ := st.GetSecret(ctx, repo.ID, "API_TOKEN")

	// Re-sealing the same plaintext must not produce the same ciphertext, or
	// an observer could tell when a value was unchanged.
	if string(second.Nonce) == firstNonce {
		t.Fatal("the nonce was reused across writes")
	}
	if string(second.Ciphertext) == firstCipher {
		t.Fatal("sealing the same value twice produced identical ciphertext")
	}
}

func TestSealedValuesAreBoundToTheirRepoAndName(t *testing.T) {
	v, st, repo, ctx := newVault(t)
	if err := v.Set(ctx, repo.ID, "API_TOKEN", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	row, err := st.GetSecret(ctx, repo.ID, "API_TOKEN")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}

	// Re-filing the same sealed bytes under a different name must not open:
	// the name is authenticated.
	row.Name = "OTHER_TOKEN"
	if _, err := v.open(row); err == nil {
		t.Fatal("a secret renamed in place should fail to decrypt")
	}

	// Nor should moving it to another repo, which is what scoping per repo
	// has to mean in practice.
	row.Name = "API_TOKEN"
	row.RepoID = "repo_elsewhere"
	if _, err := v.open(row); err == nil {
		t.Fatal("a secret moved to another repo should fail to decrypt")
	}
}

func TestAnotherKeyCannotOpenTheVault(t *testing.T) {
	v, st, repo, ctx := newVault(t)
	if err := v.Set(ctx, repo.ID, "API_TOKEN", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	otherKey := make([]byte, KeySize)
	otherKey[0] = 1
	other, err := NewVault(st, otherKey)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	_, err = other.Get(ctx, repo.ID, "API_TOKEN")
	if err == nil {
		t.Fatal("a different master key should not decrypt existing secrets")
	}
	// The operator needs to understand what went wrong.
	if !strings.Contains(err.Error(), "master key") {
		t.Fatalf("error should point at the key: %v", err)
	}
}

func TestNameValidation(t *testing.T) {
	v, _, repo, ctx := newVault(t)
	for _, name := range []string{"lower_case", "1LEADING_DIGIT", "HAS-HYPHEN", "HAS SPACE", ""} {
		if err := v.Set(ctx, repo.ID, name, "v"); err == nil {
			t.Errorf("Set(%q) should have been rejected", name)
		}
	}
	for _, name := range []string{"API_TOKEN", "_PRIVATE", "PORT2"} {
		if err := v.Set(ctx, repo.ID, name, "v"); err != nil {
			t.Errorf("Set(%q) = %v, want success", name, err)
		}
	}
	if err := v.Set(ctx, "", "API_TOKEN", "v"); err == nil {
		t.Error("Set without a repo id should be rejected")
	}
}

func TestEnvironmentIsInheritedByEveryForkInTheRepo(t *testing.T) {
	v, _, repo, ctx := newVault(t)
	want := map[string]string{
		"DATABASE_URL": "postgres://localhost/app",
		"API_TOKEN":    "t0ken",
		"REDIS_URL":    "redis://localhost:6379",
	}
	for name, value := range want {
		if err := v.Set(ctx, repo.ID, name, value); err != nil {
			t.Fatalf("Set(%s): %v", name, err)
		}
	}

	// A fork is handed its repo's whole secret set without naming any of them.
	env, err := v.Environment(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	if len(env) != len(want) {
		t.Fatalf("environment has %d entries, want %d", len(env), len(want))
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("env[%s] = %q, want %q", name, env[name], value)
		}
	}
}

func TestSecretsDoNotLeakAcrossRepos(t *testing.T) {
	v, st, repo, ctx := newVault(t)
	other := &domain.Repo{UserID: repo.UserID, Name: "other", RemoteURL: "git@example.com:other.git"}
	if err := st.CreateRepo(ctx, other); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	if err := v.Set(ctx, repo.ID, "API_TOKEN", "mine"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	env, err := v.Environment(ctx, other.ID)
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("another repo inherited %v", env)
	}
	if _, err := v.Get(ctx, other.ID, "API_TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get across repos = %v, want ErrNotFound", err)
	}
}

func TestNamesDoesNotDecrypt(t *testing.T) {
	v, _, repo, ctx := newVault(t)
	for _, name := range []string{"B_TOKEN", "A_TOKEN"} {
		if err := v.Set(ctx, repo.ID, name, "value"); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	names, err := v.Names(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 2 || names[0] != "A_TOKEN" || names[1] != "B_TOKEN" {
		t.Fatalf("Names = %v, want a sorted name list", names)
	}
}

func TestDelete(t *testing.T) {
	v, _, repo, ctx := newVault(t)
	if err := v.Set(ctx, repo.ID, "API_TOKEN", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := v.Delete(ctx, repo.ID, "API_TOKEN"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get(ctx, repo.ID, "API_TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := v.Delete(ctx, repo.ID, "API_TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestEmptyValuesAreSupported(t *testing.T) {
	v, _, repo, ctx := newVault(t)
	if err := v.Set(ctx, repo.ID, "OPTIONAL_FLAG", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := v.Get(ctx, repo.ID, "OPTIONAL_FLAG")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Fatalf("Get = %q, want an empty value", got)
	}
}
