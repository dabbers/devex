package install

import (
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/appspec"
)

// needsEverything declares one of each requirement, so a test can withhold
// exactly one thing and see it reported.
func needsEverything() *appspec.Spec {
	return &appspec.Spec{
		App: "kitchensink", Version: "1.0.0", Platform: 1,
		Requires: appspec.Requires{
			Database: appspec.Database{Engine: "postgres"},
			Identity: true, Mail: true, Storage: true,
			Realtime: true, Search: true, Payments: "required",
		},
		Secrets: []appspec.Secret{
			{Name: "TURNSTILE_SECRET", Description: "Spam protection.", Required: true},
			{Name: "ANALYTICS_KEY", Description: "Optional analytics.", Required: false},
		},
	}
}

func everythingProvided() Provided {
	return Provided{
		DatabaseURL: "postgres://app@db/app",
		Identity:    Identity{Issuer: "https://id.example", ClientID: "cid", ClientSecret: "shh"},
		MailURL:     "smtp://mail.example",
		Storage:     Storage{Endpoint: "https://s3.example", Bucket: "b", Prefix: "p", AccessKey: "ak", SecretKey: "sk"},
		RealtimeURL: "wss://sync.example",
		SearchURL:   "https://search.example",
		Payments:    Payments{AccountID: "acct_1", SecretKey: "sk_live"},
		Secrets:     map[string]string{"TURNSTILE_SECRET": "t"},
	}
}

func TestAnAppGetsWhatItDeclared(t *testing.T) {
	t.Parallel()

	env, missing := Environment(needsEverything(), everythingProvided())
	if len(missing) != 0 {
		t.Fatalf("nothing should be missing: %v", missing)
	}
	for _, name := range []string{
		"DATABASE_URL", "OIDC_ISSUER", "OIDC_CLIENT_ID", "SMTP_URL",
		"STORAGE_BUCKET", "REALTIME_URL", "SEARCH_URL", "STRIPE_ACCOUNT_ID",
		"TURNSTILE_SECRET", TrustedProxyEnv,
	} {
		if env[name] == "" {
			t.Errorf("%s was not injected", name)
		}
	}
	// Declared and not supplied, and not required: absent rather than empty.
	if _, present := env["ANALYTICS_KEY"]; present {
		t.Error("an optional secret nobody supplied was injected anyway")
	}
}

// TestAnAppGetsOnlyWhatItDeclared keeps the blast radius of a compromised app
// to the blocks it actually uses. An app that never asked for storage has no
// business holding storage credentials.
func TestAnAppGetsOnlyWhatItDeclared(t *testing.T) {
	t.Parallel()

	modest := &appspec.Spec{
		App: "forms", Version: "0.1.0", Platform: 1,
		Requires: appspec.Requires{Database: appspec.Database{Engine: "postgres"}},
	}
	env, missing := Environment(modest, everythingProvided())
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	for _, name := range []string{
		"OIDC_CLIENT_SECRET", "SMTP_URL", "STORAGE_SECRET_KEY",
		"REALTIME_URL", "SEARCH_URL", "STRIPE_SECRET_KEY",
	} {
		if _, leaked := env[name]; leaked {
			t.Errorf("%s reached an app that never declared it", name)
		}
	}
	if env["DATABASE_URL"] == "" {
		t.Error("the one thing it did declare was not provided")
	}
}

// TestRefusingToStartBeatsFailingAtThreeInTheMorning: an app started without
// what it declared does not fail at boot, it fails when the first notification
// does not arrive, and by then nobody connects the two.
func TestRefusingToStartBeatsFailingAtThreeInTheMorning(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		withhold func(*Provided)
		names    string
	}{
		"no database": {func(p *Provided) { p.DatabaseURL = "" }, "database"},
		"no identity": {func(p *Provided) { p.Identity = Identity{} }, "identity"},
		"no mail":     {func(p *Provided) { p.MailURL = "" }, "mail"},
		"no storage":  {func(p *Provided) { p.Storage = Storage{} }, "storage"},
		"no realtime": {func(p *Provided) { p.RealtimeURL = "" }, "realtime"},
		"no search":   {func(p *Provided) { p.SearchURL = "" }, "search"},
		"no payments": {func(p *Provided) { p.Payments = Payments{} }, "payments"},
		"no secret":   {func(p *Provided) { p.Secrets = nil }, "TURNSTILE_SECRET"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			provided := everythingProvided()
			test.withhold(&provided)

			spec := needsEverything()
			_, missing := Environment(spec, provided)
			if len(missing) == 0 {
				t.Fatalf("the app would have started without %s", test.names)
			}
			err := Refuse(spec, missing)
			if err == nil {
				t.Fatal("Refuse returned no error for unmet requirements")
			}
			if !strings.Contains(err.Error(), test.names) {
				t.Fatalf("the refusal does not name %s:\n%v", test.names, err)
			}
			// A refusal a person has to decode is a refusal that gets ignored.
			if !strings.Contains(err.Error(), "cannot start") {
				t.Fatalf("unhelpful refusal:\n%v", err)
			}
		})
	}
}

// TestEveryUnmetRequirementIsReportedAtOnce: fixing an installation one
// refusal at a time is a slow loop, and the platform already knows them all.
func TestEveryUnmetRequirementIsReportedAtOnce(t *testing.T) {
	t.Parallel()

	spec := needsEverything()
	_, missing := Environment(spec, Provided{})
	if len(missing) < 7 {
		t.Fatalf("only %d requirements were reported: %v", len(missing), missing)
	}

	err := Refuse(spec, missing)
	for _, expected := range []string{"database", "identity", "mail", "storage", "payments"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the report omits %s:\n%v", expected, err)
		}
	}
}

// TestAnOptionalBlockIsNotARequirement: payments declared optional means the
// app works without a connected account and uses one when it is there.
func TestAnOptionalBlockIsNotARequirement(t *testing.T) {
	t.Parallel()

	spec := &appspec.Spec{
		App: "forms", Version: "0.1.0", Platform: 1,
		Requires: appspec.Requires{Payments: "optional"},
	}

	env, missing := Environment(spec, Provided{})
	if len(missing) != 0 {
		t.Fatalf("an optional block was treated as required: %v", missing)
	}
	if _, present := env["STRIPE_ACCOUNT_ID"]; present {
		t.Error("payments credentials appeared with no account connected")
	}

	env, missing = Environment(spec, Provided{Payments: Payments{AccountID: "acct_1", SecretKey: "sk"}})
	if len(missing) != 0 || env["STRIPE_ACCOUNT_ID"] != "acct_1" {
		t.Fatalf("a connected account was not used: env=%v missing=%v", env, missing)
	}
}

// TestTheRealManifestIsSatisfiable runs the check against the app that
// actually exists, rather than a fixture written to pass.
func TestTheRealManifestIsSatisfiable(t *testing.T) {
	t.Parallel()

	spec, err := appspec.Load("../../apps/forms")
	if err != nil {
		t.Fatalf("loading the Forms manifest: %v", err)
	}

	// What the platform can provide today: a database. Mail and storage are
	// blocks that do not exist yet, so Forms must refuse to start -- and the
	// refusal has to say which ones.
	_, missing := Environment(spec, Provided{DatabaseURL: "postgres://app@db/app"})
	refusal := Refuse(spec, missing)
	if refusal == nil {
		t.Fatal("Forms started with blocks that do not exist yet")
	}
	for _, expected := range []string{"identity", "mail", "storage"} {
		if !strings.Contains(refusal.Error(), expected) {
			t.Errorf("the refusal omits %s:\n%v", expected, refusal)
		}
	}

	// And with everything it declares, it starts.
	env, missing := Environment(spec, everythingProvided())
	if len(missing) != 0 {
		t.Fatalf("Forms could not be satisfied: %v", missing)
	}
	if env["DATABASE_URL"] == "" || env[TrustedProxyEnv] != "1" {
		t.Fatalf("env = %v", env)
	}
}
