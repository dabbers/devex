package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/dabbers/devex/internal/domain"
)

// stubTokens stands in for the identity provider.
type stubTokens struct {
	token string
	err   error
	asked []string
}

func (s *stubTokens) Token(_ context.Context, userID string) (string, error) {
	s.asked = append(s.asked, userID)
	return s.token, s.err
}

func newGitEnvManager(tokens GitTokens) *Manager {
	return &Manager{gitTokens: tokens, opts: Options{}.withDefaults()}
}

// TestAUsersTokenReachesGitAndNotTheMachine is the point of resolving a
// credential per account rather than putting one in the repo's secrets. The
// secrets are injected into the instance and so are readable by the agent and
// by every service a project runs; a person's GitHub token is not a repo
// secret and must not end up there.
func TestAUsersTokenReachesGitAndNotTheMachine(t *testing.T) {
	t.Parallel()

	tokens := &stubTokens{token: "gho_personal"}
	manager := newGitEnvManager(tokens)

	machineEnv := map[string]string{"DATABASE_URL": "postgres://localhost/app"}
	gitEnv := manager.gitEnv(context.Background(), &domain.Workspace{ID: "ws_1", UserID: "usr_1"}, machineEnv)

	if gitEnv[GitTokenEnv] != "gho_personal" {
		t.Fatalf("git ran without the user's credential: %v", gitEnv)
	}
	if _, leaked := machineEnv[GitTokenEnv]; leaked {
		t.Fatal("the credential was added to the environment the machine and its services get")
	}
	if gitEnv["DATABASE_URL"] != machineEnv["DATABASE_URL"] {
		t.Fatal("the repo's own secrets did not survive into the git environment")
	}
	if len(tokens.asked) != 1 || tokens.asked[0] != "usr_1" {
		t.Fatalf("the credential was resolved for %v, want the workspace's owner", tokens.asked)
	}
}

// TestARepoSecretWinsOverTheAccountsConnection: setting GIT_TOKEN on the repo
// is an explicit choice, and it is what lets a repo be cloned by a machine
// account rather than by whoever happened to open the workspace.
func TestARepoSecretWinsOverTheAccountsConnection(t *testing.T) {
	t.Parallel()

	tokens := &stubTokens{token: "gho_personal"}
	manager := newGitEnvManager(tokens)

	env := map[string]string{GitTokenEnv: "ghp_the_repos_own"}
	gitEnv := manager.gitEnv(context.Background(), &domain.Workspace{ID: "ws_1", UserID: "usr_1"}, env)

	if gitEnv[GitTokenEnv] != "ghp_the_repos_own" {
		t.Fatalf("token = %q, want the repo's own", gitEnv[GitTokenEnv])
	}
	if len(tokens.asked) != 0 {
		t.Fatal("the identity provider was consulted although the repo carried a token")
	}
}

// TestNoCredentialIsNotAFailure: a public repository clones without one, and
// a private one fails later with git's own message, which says more than
// anything that could be guessed here.
func TestNoCredentialIsNotAFailure(t *testing.T) {
	t.Parallel()

	for name, tokens := range map[string]*stubTokens{
		"no connection": {err: errors.New("no github connection")},
		"empty token":   {token: ""},
	} {
		t.Run(name, func(t *testing.T) {
			manager := newGitEnvManager(tokens)
			env := map[string]string{"A": "b"}
			gitEnv := manager.gitEnv(context.Background(), &domain.Workspace{ID: "ws_1"}, env)

			if _, present := gitEnv[GitTokenEnv]; present {
				t.Fatal("a credential appeared from nowhere")
			}
			if gitEnv["A"] != "b" {
				t.Fatal("the environment was lost")
			}
		})
	}
}

// TestADeploymentWithNoProviderStillStartsWorkspaces covers the single-user
// path, where nothing resolves credentials at all.
func TestADeploymentWithNoProviderStillStartsWorkspaces(t *testing.T) {
	t.Parallel()

	manager := newGitEnvManager(nil)
	env := map[string]string{"A": "b"}
	if got := manager.gitEnv(context.Background(), &domain.Workspace{ID: "ws_1"}, env); got["A"] != "b" {
		t.Fatalf("env = %v", got)
	}
}
