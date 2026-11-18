package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dabbers/devex/internal/auth"
	"github.com/dabbers/devex/internal/store"
)

// A token issued by this deployment authenticates without a provider, and
// stops doing so the moment it is revoked.
func TestAPlatformTokenAuthenticatesAndThenDoesNot(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "dabberz.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	owner, err := db.EnsureUserByExternalID(ctx, "prov|owner", "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUserByExternalID: %v", err)
	}

	// A provider that refuses everything, so a request that succeeds below
	// can only have succeeded on the token.
	translator, err := New(rejecting{}, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	translator = translator.WithTokens(db)

	issued, plain, err := db.CreateToken(ctx, owner.ID, "a test")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	who, err := translator.Visitor(ctx, bearing(plain))
	if err != nil {
		t.Fatalf("a freshly issued token was refused: %v", err)
	}
	if who.ID != owner.ID {
		t.Fatalf("the token authenticated as %s, want %s", who.ID, owner.ID)
	}

	if err := db.RevokeToken(ctx, owner.ID, issued.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := translator.Visitor(ctx, bearing(plain)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("a revoked token was answered with %v, want unauthenticated", err)
	}
}

// A token that was never issued is refused rather than quietly handed to the
// provider, which would report it as the wrong kind of failure.
func TestAnUnknownPlatformTokenIsRefusedRatherThanPassedOn(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "dabberz.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	translator, err := New(rejecting{}, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	translator = translator.WithTokens(db)

	_, err = translator.Visitor(ctx, bearing(store.TokenPrefix+"not-a-real-token"))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("an unknown token was answered with %v, want unauthenticated", err)
	}
}

// The token is not recoverable from the store, which is the point of hashing
// it: a stolen copy of this table is worth nothing on its own.
func TestTheTokenItselfIsNotStored(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "dabberz.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	owner, err := db.EnsureUserByExternalID(ctx, "prov|owner", "owner@example.com")
	if err != nil {
		t.Fatalf("EnsureUserByExternalID: %v", err)
	}
	_, plain, err := db.CreateToken(ctx, owner.ID, "a test")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	listed, err := db.ListTokens(ctx, owner.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, token := range listed {
		if token.Name == plain || token.ID == plain {
			t.Fatal("the token itself came back from the store")
		}
	}
}

func bearing(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// rejecting stands in for a provider that accepts nothing.
type rejecting struct{}

func (rejecting) Name() string { return "rejecting" }
func (rejecting) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return nil, auth.ErrNoCredentials
}
func (rejecting) PublicConfig() map[string]any { return map[string]any{} }
