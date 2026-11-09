package gitauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// users is a stand-in for the store's user lookup.
type users map[string]string

func (u users) ExternalID(_ context.Context, userID string) (string, error) {
	return u[userID], nil
}

// provider is a stand-in Clerk backend API.
type provider struct {
	server *httptest.Server
	calls  atomic.Int64

	// tokenStatus and tokenBody are what the token endpoint answers with.
	tokenStatus int
	tokenBody   string
	// userBody is what the user endpoint answers with.
	userBody string
}

func newProvider(t *testing.T) *provider {
	t.Helper()

	p := &provider{
		tokenStatus: http.StatusOK,
		tokenBody:   `[{"token":"gho_realtoken","provider":"github","scopes":["repo"]}]`,
		userBody:    `{"external_accounts":[{"provider":"oauth_github","username":"dabbers"}]}`,
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "/oauth_access_tokens/") {
			p.calls.Add(1)
			w.WriteHeader(p.tokenStatus)
			_, _ = w.Write([]byte(p.tokenBody))
			return
		}
		_, _ = w.Write([]byte(p.userBody))
	}))
	t.Cleanup(p.server.Close)
	return p
}

func newResolver(t *testing.T, p *provider) *Resolver {
	t.Helper()
	return New(Options{
		SecretKey: "sk_test_key",
		Users:     users{"usr_1": "user_clerk1"},
		Endpoint:  p.server.URL,
	})
}

func TestAConnectedAccountYieldsItsToken(t *testing.T) {
	t.Parallel()

	resolver := newResolver(t, newProvider(t))
	token, err := resolver.Token(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "gho_realtoken" {
		t.Fatalf("token = %q", token)
	}
}

func TestBothShapesOfTheTokenListAreRead(t *testing.T) {
	t.Parallel()

	// The provider has spelled this list both ways. Reading only one means a
	// change at their end silently stops every clone, with no error that says
	// so.
	for name, body := range map[string]string{
		"a bare array":        `[{"token":"gho_a","provider":"github"}]`,
		"a paginated object":  `{"data":[{"token":"gho_b","provider":"github"}],"total_count":1}`,
		"an object with none": `{"data":[],"total_count":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t)
			p.tokenBody = body
			resolver := newResolver(t, p)

			token, err := resolver.Token(context.Background(), "usr_1")
			switch {
			case strings.Contains(body, `"gho_`):
				if err != nil || !strings.HasPrefix(token, "gho_") {
					t.Fatalf("token = %q, err = %v", token, err)
				}
			default:
				if !errors.Is(err, ErrNotConnected) {
					t.Fatalf("err = %v, want ErrNotConnected", err)
				}
			}
		})
	}
}

// TestSharedDevelopmentCredentialsAreNamedAsTheReason is the case this whole
// status surface exists for: the connection is real, the provider simply will
// not hand over its token, and "not connected" would send the user to connect
// something that is already connected.
func TestSharedDevelopmentCredentialsAreNamedAsTheReason(t *testing.T) {
	t.Parallel()

	for _, code := range []string{codeSharedNotSupported, codeSharedStored} {
		p := newProvider(t)
		p.tokenStatus = http.StatusUnprocessableEntity
		p.tokenBody = `{"errors":[{"code":"` + code + `","message":"not supported"}]}`
		resolver := newResolver(t, p)

		if _, err := resolver.Token(context.Background(), "usr_1"); !errors.Is(err, ErrSharedCredentials) {
			t.Fatalf("%s: err = %v, want ErrSharedCredentials", code, err)
		}

		link := resolver.Status(context.Background(), "usr_1")
		if !link.Connected {
			t.Errorf("%s: the connection was reported as absent", code)
		}
		if link.Usable {
			t.Errorf("%s: an unusable connection was reported as usable", code)
		}
		if !strings.Contains(link.Blocked, "custom credentials") {
			t.Errorf("%s: the explanation does not say what to do: %q", code, link.Blocked)
		}
	}
}

func TestAnAccountWithNoConnectionIsReportedAsSuch(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	p.tokenStatus = http.StatusNotFound
	p.tokenBody = `{"errors":[{"code":"resource_not_found","message":"no tokens"}]}`
	p.userBody = `{"external_accounts":[]}`
	resolver := newResolver(t, p)

	if _, err := resolver.Token(context.Background(), "usr_1"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	link := resolver.Status(context.Background(), "usr_1")
	if link.Connected || link.Usable {
		t.Fatalf("link = %+v, want neither connected nor usable", link)
	}
	if !strings.Contains(link.Blocked, "Connect a GitHub account") {
		t.Fatalf("unhelpful explanation: %q", link.Blocked)
	}
}

func TestStatusNamesTheAccountAndItsScopes(t *testing.T) {
	t.Parallel()

	resolver := newResolver(t, newProvider(t))
	link := resolver.Status(context.Background(), "usr_1")

	if !link.Configured || !link.Connected || !link.Usable {
		t.Fatalf("link = %+v", link)
	}
	if link.Account != "dabbers" {
		t.Errorf("account = %q, want the GitHub username", link.Account)
	}
	if len(link.Scopes) != 1 || link.Scopes[0] != "repo" {
		t.Errorf("scopes = %v", link.Scopes)
	}
	if link.Blocked != "" {
		t.Errorf("a working connection carried a complaint: %q", link.Blocked)
	}
}

// TestTheStatusCarriesNoCredential: this is served to a browser, and a token
// that reached the page would end up in a cache, a screenshot or a log.
func TestTheStatusCarriesNoCredential(t *testing.T) {
	t.Parallel()

	resolver := newResolver(t, newProvider(t))
	link := resolver.Status(context.Background(), "usr_1")

	rendered := link.Account + strings.Join(link.Scopes, " ") + link.Blocked
	if strings.Contains(rendered, "gho_") {
		t.Fatalf("the status carried a token: %+v", link)
	}
}

func TestATokenIsReusedBrieflyAndCanBeForgotten(t *testing.T) {
	t.Parallel()

	// One workspace start runs several git commands. Asking the provider for
	// a token on each is a request per command, and a rate limit away from a
	// clone that fails halfway.
	p := newProvider(t)
	resolver := newResolver(t, p)

	for range 5 {
		if _, err := resolver.Token(context.Background(), "usr_1"); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if calls := p.calls.Load(); calls != 1 {
		t.Fatalf("the provider was asked %d times, want 1", calls)
	}

	// Someone who has just reconnected must not be answered from what was
	// remembered about the connection they replaced.
	resolver.Forget("usr_1")
	if _, err := resolver.Token(context.Background(), "usr_1"); err != nil {
		t.Fatalf("Token after Forget: %v", err)
	}
	if calls := p.calls.Load(); calls != 2 {
		t.Fatalf("the provider was asked %d times after forgetting, want 2", calls)
	}
}

func TestACachedTokenExpires(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	clock := time.Now()
	resolver := New(Options{
		SecretKey: "sk_test_key",
		Users:     users{"usr_1": "user_clerk1"},
		Endpoint:  p.server.URL,
		Now:       func() time.Time { return clock },
	})

	if _, err := resolver.Token(context.Background(), "usr_1"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock = clock.Add(tokenTTL + time.Second)
	if _, err := resolver.Token(context.Background(), "usr_1"); err != nil {
		t.Fatalf("Token after expiry: %v", err)
	}
	if calls := p.calls.Load(); calls != 2 {
		t.Fatalf("the provider was asked %d times, want 2: a stale token fails a clone minutes later", calls)
	}
}

func TestWithoutAProviderCredentialNothingPretendsToWork(t *testing.T) {
	t.Parallel()

	resolver := New(Options{Users: users{"usr_1": "user_clerk1"}})
	if resolver.Configured() {
		t.Fatal("a resolver with no secret key reported itself configured")
	}
	if _, err := resolver.Token(context.Background(), "usr_1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}

	link := resolver.Status(context.Background(), "usr_1")
	if link.Configured || link.Usable {
		t.Fatalf("link = %+v", link)
	}
	if link.Blocked == "" {
		t.Fatal("an unconfigured deployment said nothing about why")
	}
}

// TestAnAccountWithNoProviderSubjectIsNotAsReportedConnected covers the
// single-user development path, where there is no identity provider at all.
func TestAnAccountWithNoProviderSubjectIsNotReportedConnected(t *testing.T) {
	t.Parallel()

	resolver := New(Options{
		SecretKey: "sk_test_key",
		Users:     users{"usr_1": ""},
		Endpoint:  newProvider(t).server.URL,
	})

	if _, err := resolver.Token(context.Background(), "usr_1"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if link := resolver.Status(context.Background(), "usr_1"); link.Connected {
		t.Fatalf("link = %+v, want not connected", link)
	}
}

// TestOneUsersTokenIsNotAnothers: the cache is keyed per account, and getting
// that wrong would hand somebody else's GitHub credential to a clone.
func TestOneUsersTokenIsNotAnothers(t *testing.T) {
	t.Parallel()

	p := newProvider(t)
	p.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "user_clerk1"):
			_, _ = w.Write([]byte(`[{"token":"gho_one","provider":"github"}]`))
		default:
			_, _ = w.Write([]byte(`[{"token":"gho_two","provider":"github"}]`))
		}
	})

	resolver := New(Options{
		SecretKey: "sk_test_key",
		Users:     users{"usr_1": "user_clerk1", "usr_2": "user_clerk2"},
		Endpoint:  p.server.URL,
	})

	first, err := resolver.Token(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("usr_1: %v", err)
	}
	second, err := resolver.Token(context.Background(), "usr_2")
	if err != nil {
		t.Fatalf("usr_2: %v", err)
	}
	if first == second {
		t.Fatalf("both accounts resolved to %q", first)
	}
}
