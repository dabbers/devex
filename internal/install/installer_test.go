package install

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/proxy/hostmux"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/local"
	"github.com/dabbers/devex/internal/workspace"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const clusterEnv = "DABBERZ_TEST_CLUSTER_DSN"

// TestInstallingFormsProducesSomethingThatWorks is the whole point of
// everything built so far, exercised in one go: a catalog entry becomes a
// repository, a database, a built binary, running services and a URL that
// answers -- and then somebody fills in the form and the answer is there.
//
// Nothing is stubbed that a customer would depend on. The database is a real
// Postgres, the repository is a real git repository, the app is really
// compiled on the machine, and the submission goes over HTTP.
func TestInstallingFormsProducesSomethingThatWorks(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles an app and provisions a database")
	}
	dsn := os.Getenv(clusterEnv)
	if dsn == "" {
		t.Skip("set " + clusterEnv + " to a superuser DSN to run the install")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	installer, db, owner, edge := newInstaller(t, dsn)

	installation, err := installer.Install(ctx, Request{
		UserID: owner.ID,
		App:    "forms",
		Name:   "recruiting",
	})
	if err != nil {
		t.Fatalf("Install: %v\nstate reason: %s", err, installation.StateReason)
	}

	if installation.State != domain.InstallationRunning {
		t.Fatalf("state = %q (%s), want running", installation.State, installation.StateReason)
	}
	if installation.Database == "" {
		t.Fatal("no database was provisioned")
	}
	if installation.AppVersion == "" {
		t.Fatal("the installation pinned no version, so an upgrade would be invisible")
	}

	// The installation is reachable at the address the platform allocated.
	// Not through a shortcut: the same URL a person would open.
	base := installation.URL
	if base == "" {
		t.Fatal("the installation has no URL")
	}
	t.Logf("installed %s at %s on database %s", installation.App, base, installation.Database)

	waitForHealth(t, ctx, edge, base, "/healthz")

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// The admin surface is refused through the public edge, because nothing
	// has established who the visitor is on this hostname. That is the
	// correct state today rather than a gap in the test: the edge strips
	// identity headers, so an unauthenticated request cannot claim one.
	refused := post(t, client, edge, base, "/forms", url.Values{"title": {"forged"}}, nil)
	if refused != http.StatusUnauthorized && refused != http.StatusServiceUnavailable {
		t.Fatalf("the admin surface answered %d to a visitor with no identity", refused)
	}

	// And a forged header does not help, which is the thing that would
	// otherwise hand somebody else's installation to anyone who tried.
	forged := post(t, client, edge, base, "/forms", url.Values{"title": {"forged"}},
		map[string]string{"X-Dabberz-User": "usr_someone_else"})
	if forged != http.StatusUnauthorized && forged != http.StatusServiceUnavailable {
		t.Fatalf("a forged identity header reached the admin surface: %d", forged)
	}

	// With an identity the platform asserts -- which is what the edge will do
	// once it can establish a session on an installation's own hostname --
	// the admin surface works. Sent to the service directly, since that is
	// the only place an assertion can come from today.
	direct := serviceAddress(t, db, installation)
	owned := post(t, client, direct, "", "/forms", url.Values{
		"title":       {"Contact us"},
		"description": {"We read everything."},
		"fields":      {"Your name | text | required\nEmail | email | required"},
	}, map[string]string{"X-Dabberz-User": owner.ID})
	if owned != http.StatusSeeOther {
		t.Fatalf("creating a form as the owner returned %d", owned)
	}

	// The public form is public, and goes through the edge like a visitor's
	// would.
	answered := post(t, client, edge, base, "/f/contact-us", url.Values{
		"your-name": {"Ada Lovelace"},
		"email":     {"ada@example.com"},
	}, nil)
	if answered != http.StatusOK {
		t.Fatalf("submitting the form returned %d", answered)
	}

	// Read it back out of the installation's own database, which proves the
	// app was talking to the one the platform provisioned for it rather than
	// to anything it found lying around.
	var stored string
	query := `SELECT data->>'your-name' FROM submissions LIMIT 1`
	if err := db.QueryRow(ctx, installation.Database, query, &stored); err != nil {
		t.Fatalf("reading the submission: %v", err)
	}
	if stored != "Ada Lovelace" {
		t.Fatalf("stored %q", stored)
	}
}

// TestInstallingTheSameNameTwiceIsRefused: one account may install an app more
// than once, which is what the name is for.
func TestInstallingTheSameNameTwiceIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("provisions a database")
	}
	dsn := os.Getenv(clusterEnv)
	if dsn == "" {
		t.Skip("set " + clusterEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	installer, _, owner, _ := newInstaller(t, dsn)

	if _, err := installer.Install(ctx, Request{UserID: owner.ID, App: "forms", Name: "support"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	_, err := installer.Install(ctx, Request{UserID: owner.ID, App: "forms", Name: "support"})
	if err == nil {
		t.Fatal("two installations were allowed to share a name")
	}
	if !strings.Contains(err.Error(), "already have an installation") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

func TestInstallingSomethingThatIsNotInTheCatalogSaysWhatIs(t *testing.T) {
	t.Parallel()

	catalog, err := OpenCatalog("../../apps")
	if err != nil {
		t.Fatalf("OpenCatalog: %v", err)
	}
	_, err = catalog.Get("whiteboard")
	if err == nil {
		t.Fatal("an app that does not exist was found")
	}
	if !strings.Contains(err.Error(), "forms") {
		t.Fatalf("the refusal does not say what is available: %v", err)
	}
}

// --------------------------------------------------------------- harness

// tracked wraps the data block so a test drops every database it made, even
// when it fails.
type tracked struct {
	block *database.Block
	store *store.Store
	t     *testing.T
}

func (d *tracked) Provision(ctx context.Context, owner string) (*database.Database, string, error) {
	db, dsn, err := d.block.Provision(ctx, owner)
	if err == nil {
		d.t.Cleanup(func() {
			if err := d.block.Drop(context.Background(), db); err != nil {
				d.t.Logf("dropping %s: %v", db.Name, err)
			}
		})
	}
	return db, dsn, err
}

func (d *tracked) Drop(ctx context.Context, db *database.Database) error {
	return d.block.Drop(ctx, db)
}

func (d *tracked) Credentials(ctx context.Context, db *database.Database) (string, error) {
	return d.block.Credentials(ctx, db)
}

func (d *tracked) Snapshot(ctx context.Context, db *database.Database, label string) (*database.Snapshot, error) {
	return d.block.Snapshot(ctx, db, label)
}

func (d *tracked) Fork(ctx context.Context, source *database.Database, owner string) (*database.Database, error) {
	forked, err := d.block.Fork(ctx, source, owner)
	if err == nil {
		d.t.Cleanup(func() {
			if err := d.block.Drop(context.Background(), forked); err != nil {
				d.t.Logf("dropping %s: %v", forked.Name, err)
			}
		})
	}
	return forked, err
}

// QueryRow reads one value out of an installation's own database.
func (d *tracked) QueryRow(ctx context.Context, name, query string, into *string) error {
	conn, err := d.block.Connect(name)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.QueryRowContext(ctx, query).Scan(into)
}

func newInstaller(t *testing.T, clusterDSN string) (*Installer, *tracked, *domain.User, string) {
	t.Helper()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dataDir := t.TempDir()

	db, err := store.Open(context.Background(), filepath.Join(dataDir, "dabberz.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	owner := &domain.User{Email: "owner@example.com"}
	if err := db.CreateUser(context.Background(), owner); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	vault, err := secrets.NewVault(db, make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatalf("secrets.NewVault: %v", err)
	}

	driver, err := local.New(local.Options{
		Root:  filepath.Join(dataDir, "vms"),
		Total: vm.Resources{VCPUs: 8, MemoryMiB: 16384, DiskGiB: 100},
	})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	// The real in-process router, served on a loopback address, so the test
	// reaches the installation the way a visitor would -- by its preview
	// hostname, through the mux that resolves it -- rather than by poking the
	// service's port directly.
	router := hostmux.New("test.invalid", quiet)
	edge := httptest.NewServer(router)
	t.Cleanup(edge.Close)

	allocator, err := preview.New(db, router, preview.Config{
		Domain: "test.invalid", Scheme: "http",
	})
	if err != nil {
		t.Fatalf("preview.New: %v", err)
	}

	// Set after the installer exists, since it is the thing that knows which
	// database a workspace belongs to.
	overrides := &lateOverrides{}
	workspaces, err := workspace.New(workspace.Deps{
		Store: db, Driver: driver, Preview: allocator, Vault: vault,
		Overrides: overrides,
	}, workspace.Options{
		Resources: vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10},
		Image:     "dabberz-golden",
		Logger:    quiet,
	})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	block, err := database.Open(database.Config{
		AdminDSN:    clusterDSN,
		SnapshotDir: filepath.Join(dataDir, "snapshots"),
		BinDir:      os.Getenv("DABBERZ_TEST_PG_BIN"),
	})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { block.Close() })
	databases := &tracked{block: block, store: db, t: t}

	catalog, err := OpenCatalog("../../apps")
	if err != nil {
		t.Fatalf("OpenCatalog: %v", err)
	}

	installer, err := New(Deps{
		Store: db, Catalog: catalog, Workspaces: workspaces,
		Vault: vault, Databases: databases, Logger: quiet,
	}, Options{
		RepoDir: filepath.Join(dataDir, "repos"),
		// What the blocks that do exist offer. Identity, mail and storage are
		// stand-ins: Forms declares them, so without something here it would
		// correctly refuse to install at all.
		Provided: Provided{
			Identity: Identity{Issuer: "https://id.test.invalid", ClientID: "dabberz", ClientSecret: "shh"},
			MailURL:  "smtp://mail.test.invalid:25",
			Storage: Storage{
				Endpoint: "https://s3.test.invalid", Bucket: "installs",
				Prefix: "forms/", AccessKey: "ak", SecretKey: "sk",
			},
		},
	})
	if err != nil {
		t.Fatalf("install.New: %v", err)
	}
	overrides.to = installer
	return installer, databases, owner, edge.URL
}

// lateOverrides breaks the circle between the workspace manager and the
// installer: the manager needs somewhere to ask which database a workspace
// belongs to, and the only thing that knows is built on top of the manager.
type lateOverrides struct{ to workspace.Overrides }

func (l *lateOverrides) For(ctx context.Context, w *domain.Workspace) (map[string]string, error) {
	if l.to == nil {
		return nil, nil
	}
	return l.to.For(ctx, w)
}

// serviceAddress is where an installation's service is actually listening, for
// the one thing that cannot go through the public edge yet.
func serviceAddress(t *testing.T, db *tracked, installation *domain.Installation) string {
	t.Helper()

	routes, err := db.store.ListPreviewRoutesFor(context.Background(), installation.WorkspaceID)
	if err != nil || len(routes) == 0 {
		t.Fatalf("no route for installation %s: %v", installation.ID, err)
	}
	return fmt.Sprintf("http://%s:%d", routes[0].UpstreamHost, routes[0].UpstreamPort)
}

// edgeOf rewrites a preview URL onto the loopback address the test's edge is
// listening on, keeping the hostname so the mux still has to route it.
func edgeOf(t *testing.T, edge, previewURL, path string) (string, string) {
	t.Helper()

	parsed, err := url.Parse(previewURL)
	if err != nil {
		t.Fatalf("unreadable preview URL %q: %v", previewURL, err)
	}
	return edge + path, parsed.Host
}

func waitForHealth(t *testing.T, ctx context.Context, edge, previewURL, path string) {
	t.Helper()

	target, host := edgeOf(t, edge, previewURL, path)
	deadline := time.Now().Add(90 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("%s never became healthy: %v", previewURL+path, err)
		}
		time.Sleep(time.Second)
	}
}

func post(t *testing.T, client *http.Client, edge, previewURL, path string, values url.Values, headers map[string]string) int {
	t.Helper()

	target, host := edge+path, ""
	if previewURL != "" {
		target, host = edgeOf(t, edge, previewURL, path)
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode())) //nolint:noctx // bounded by the client timeout
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", previewURL+path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		t.Fatalf("POST %s = %d: %s", previewURL+path, resp.StatusCode, body)
	}
	return resp.StatusCode
}

// QueryRowInt reads one number out of an installation's own database.
func (d *tracked) QueryRowInt(ctx context.Context, name, query string, into *int) error {
	conn, err := d.block.Connect(name)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.QueryRowContext(ctx, query).Scan(into)
}
