package previewpass

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

var owner = &domain.User{ID: "usr_owner", Email: "owner@example.com"}

func issuer(t *testing.T) *Issuer {
	t.Helper()
	i, err := NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestAPassNamesItsOwnerAtItsHost(t *testing.T) {
	i := issuer(t)
	pass, err := i.Mint("App-Change.ondabberz.link", owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// However the request spells the host, as long as it is the same one.
	who, err := i.Check(pass, "app-change.ondabberz.link:443")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if who.ID != owner.ID || who.Email != owner.Email {
		t.Fatalf("the pass names %+v, want %+v", who, owner)
	}
}

// TestAPassIsGoodForNothingElse is the reason a pass exists instead of the
// owner's token: whatever it leaks to, it is useless there.
func TestAPassIsGoodForNothingElse(t *testing.T) {
	i := issuer(t)
	pass, err := i.Mint("app-change.ondabberz.link", owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := i.Check(pass, "app.ondabberz.link"); !errors.Is(err, ErrWrongHost) {
		t.Fatalf("a pass for one preview was accepted at another: %v", err)
	}

	other := issuer(t)
	if _, err := other.Check(pass, "app-change.ondabberz.link"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a pass was accepted by a daemon that did not issue it: %v", err)
	}

	// Altering what it names breaks it, rather than naming someone else.
	body, signature, _ := strings.Cut(pass, ".")
	forged := strings.ToUpper(body[:1]) + strings.ToLower(body[1:]) + "." + signature
	if forged != pass {
		if _, err := i.Check(forged, "app-change.ondabberz.link"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an altered pass was accepted: %v", err)
		}
	}
	for _, junk := range []string{"", ".", "nodot", pass + "x", "." + signature} {
		if _, err := i.Check(junk, "app-change.ondabberz.link"); err == nil {
			t.Fatalf("%q was accepted as a pass", junk)
		}
	}
}

func TestAPassExpires(t *testing.T) {
	i := issuer(t)
	start := time.Now()
	i.now = func() time.Time { return start }
	pass, err := i.Mint("app-change.ondabberz.link", owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	i.now = func() time.Time { return start.Add(time.Minute) }
	if _, err := i.Check(pass, "app-change.ondabberz.link"); !errors.Is(err, ErrExpired) {
		t.Fatalf("a pass was accepted after it expired: %v", err)
	}
}

func TestMintRefusesAnIncompletePass(t *testing.T) {
	i := issuer(t)
	for name, mint := range map[string]func() (string, error){
		"no host":     func() (string, error) { return i.Mint("", owner, time.Minute) },
		"no user":     func() (string, error) { return i.Mint("h", nil, time.Minute) },
		"no user id":  func() (string, error) { return i.Mint("h", &domain.User{}, time.Minute) },
		"no lifetime": func() (string, error) { return i.Mint("h", owner, 0) },
	} {
		if _, err := mint(); err == nil {
			t.Errorf("%s: minted anyway", name)
		}
	}
	if _, err := New(make([]byte, MinKeySize-1)); err == nil {
		t.Error("a short key was accepted")
	}
}
