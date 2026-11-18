// Package install turns a manifest into a running app.
//
// An app declares what it needs; the platform provides it. This is the seam
// between those two sentences: it checks that everything the manifest requires
// is actually on offer, and assembles the environment the app is started with.
//
// The check matters more than the assembly. An app that declares it needs mail
// and is started without it does not fail at boot -- it fails at three in the
// morning when the first notification does not arrive, by which time nobody
// connects the two. Refusing to start is the kinder failure.
package install

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dabbers/devex/internal/appspec"
	"github.com/dabbers/devex/internal/proxy"
)

// The platform-to-app interface that is not a provisioned URL.
//
// Identity is terminated by the platform and forwarded, which is only safe
// while the app is unreachable except through it. TrustedProxyEnv is the
// platform asserting exactly that, and an app that does not see it must refuse
// to serve anything behind an identity rather than assume the headers are
// real.
const (
	TrustedProxyEnv = "DABBERZ_TRUSTED_PROXY"
	UserHeader      = proxy.UserHeader
	EmailHeader     = proxy.EmailHeader
)

// Provided is what the platform's blocks have made available for one
// installation. Empty fields mean "this block did not provide anything",
// which is only a problem if the manifest said it was needed.
type Provided struct {
	// DatabaseURL comes from the data block.
	DatabaseURL string
	// Identity is the OIDC configuration an app authenticates against. The
	// app never implements login, so without this its admin surface has no
	// way to tell who is asking.
	Identity Identity
	// MailURL is an SMTP endpoint owned by the platform, which also owns the
	// sending domain and its reputation.
	MailURL string
	// Storage is an S3-compatible bucket scoped to this installation.
	Storage Storage
	// RealtimeURL is the shared sync fabric.
	RealtimeURL string
	// SearchURL is the shared index.
	SearchURL string
	// Payments is the customer's own connected Stripe account. The platform
	// brokers the connection and never touches the funds.
	Payments Payments
	// Secrets are the values the customer supplied for the names the manifest
	// declared. Values, by necessity: this is the one place they exist
	// outside the vault, on their way into the process that needs them.
	Secrets map[string]string
}

// Identity is the OIDC configuration.
type Identity struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// Storage is an object store scoped to one installation.
type Storage struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
}

// Payments is a connected Stripe account.
type Payments struct {
	AccountID string
	SecretKey string
}

// Missing is a requirement the manifest declared and the platform did not
// meet. Returned rather than logged, because the right response is to refuse
// to start the installation.
type Missing struct {
	Requirement string
	Why         string
}

func (m Missing) String() string { return m.Requirement + ": " + m.Why }

// Environment assembles what an app is started with, or reports what is
// missing.
//
// Nothing is injected that the manifest did not ask for. An app that never
// declared storage does not get storage credentials in its environment, which
// keeps the blast radius of a compromised app to the blocks it actually uses.
func Environment(spec *appspec.Spec, provided Provided) (map[string]string, []Missing) {
	env := map[string]string{}
	var missing []Missing

	need := func(condition bool, requirement, why string) bool {
		if !condition {
			missing = append(missing, Missing{Requirement: requirement, Why: why})
			return false
		}
		return true
	}

	if spec.Requires.Database.Engine != "" {
		if need(provided.DatabaseURL != "", "database",
			"the app stores data and no database was provisioned") {
			env["DATABASE_URL"] = provided.DatabaseURL
		}
	}

	if spec.Requires.Identity {
		if need(provided.Identity.Issuer != "" && provided.Identity.ClientID != "", "identity",
			"the app does not implement login and was given nothing to authenticate against") {
			env["OIDC_ISSUER"] = provided.Identity.Issuer
			env["OIDC_CLIENT_ID"] = provided.Identity.ClientID
			env["OIDC_CLIENT_SECRET"] = provided.Identity.ClientSecret
		}
	}

	if spec.Requires.Mail {
		if need(provided.MailURL != "", "mail",
			"the app sends mail and no transport was provided") {
			env["SMTP_URL"] = provided.MailURL
		}
	}

	if spec.Requires.Storage {
		if need(provided.Storage.Bucket != "", "storage",
			"the app stores files and no bucket was provisioned") {
			env["STORAGE_ENDPOINT"] = provided.Storage.Endpoint
			env["STORAGE_BUCKET"] = provided.Storage.Bucket
			env["STORAGE_PREFIX"] = provided.Storage.Prefix
			env["STORAGE_ACCESS_KEY"] = provided.Storage.AccessKey
			env["STORAGE_SECRET_KEY"] = provided.Storage.SecretKey
		}
	}

	if spec.Requires.Realtime {
		if need(provided.RealtimeURL != "", "realtime",
			"the app synchronises documents and the sync fabric was not reachable") {
			env["REALTIME_URL"] = provided.RealtimeURL
		}
	}

	if spec.Requires.Search {
		if need(provided.SearchURL != "", "search",
			"the app indexes its content and no search service was provided") {
			env["SEARCH_URL"] = provided.SearchURL
		}
	}

	if spec.Requires.Payments == "required" {
		need(provided.Payments.AccountID != "", "payments",
			"the app takes money and no Stripe account is connected")
	}
	if provided.Payments.AccountID != "" && spec.Requires.Payments != "" {
		env["STRIPE_ACCOUNT_ID"] = provided.Payments.AccountID
		env["STRIPE_SECRET_KEY"] = provided.Payments.SecretKey
	}

	for _, secret := range spec.Secrets {
		value, supplied := provided.Secrets[secret.Name]
		if secret.Required && !need(supplied && value != "", "secret "+secret.Name,
			secret.Description) {
			continue
		}
		if supplied {
			env[secret.Name] = value
		}
	}

	// The platform tells the app it is unreachable except through it, which is
	// what makes the identity headers it forwards trustworthy. One name for
	// every app: an app inventing its own is an app that can be started
	// without it by accident.
	env[TrustedProxyEnv] = "1"

	sort.Slice(missing, func(i, j int) bool { return missing[i].Requirement < missing[j].Requirement })
	return env, missing
}

// Refuse turns a list of unmet requirements into one error worth reading.
func Refuse(spec *appspec.Spec, missing []Missing) error {
	if len(missing) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(missing))
	for _, one := range missing {
		reasons = append(reasons, one.String())
	}
	return fmt.Errorf("install: %s %s cannot start, because the platform did not provide what it declared:\n  - %s",
		spec.App, spec.Version, strings.Join(reasons, "\n  - "))
}
