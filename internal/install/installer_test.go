package install

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/auth"
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

	installer, db, owner, edge, visiting := newInstaller(t, dsn)

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

	// The admin surface is refused through the public edge to a visitor the
	// platform cannot name.
	refused := post(t, client, edge, base, "/forms", url.Values{"title": {"forged"}}, nil)
	if refused != http.StatusUnauthorized && refused != http.StatusServiceUnavailable {
		t.Fatalf("the admin surface answered %d to a visitor with no identity", refused)
	}

	// And a forged header does not help, which is the thing that would
	// otherwise hand somebody else's installation to anyone who tried. Both
	// halves of the edge's identity handling are being exercised here: it
	// strips what the visitor sent, and asserts only what it resolved
	// itself -- which for this request is nothing.
	forged := post(t, client, edge, base, "/forms", url.Values{"title": {"forged"}},
		map[string]string{"X-Dabberz-User": owner.ID})
	if forged != http.StatusUnauthorized && forged != http.StatusServiceUnavailable {
		t.Fatalf("a forged identity header reached the admin surface: %d", forged)
	}

	// Signed in as the owner, through the same public hostname, the admin
	// surface works -- because the edge resolves the visitor and states who
	// they are. Nothing here talks to the service directly.
	visiting.be(owner)
	owned := post(t, client, edge, base, "/forms", url.Values{
		"title":       {"Contact us"},
		"description": {"We read everything."},
		"fields":      {"Your name | text | required\nEmail | email | required"},
		"audience":    {"public"},
	}, nil)
	if owned != http.StatusSeeOther {
		t.Fatalf("creating a form as the owner returned %d", owned)
	}

	// A signed-in stranger is not the owner, and the edge asserting their
	// real identity is exactly what lets the app tell them apart.
	stranger, err := db.store.EnsureUserByExternalID(ctx, "test:stranger", "stranger@example.invalid")
	if err != nil {
		t.Fatalf("creating a stranger: %v", err)
	}
	visiting.be(stranger)
	intruded := post(t, client, edge, base, "/forms", url.Values{"title": {"theirs"}}, nil)
	if intruded != http.StatusUnauthorized && intruded != http.StatusForbidden &&
		intruded != http.StatusNotFound {
		t.Fatalf("a signed-in stranger reached another installation's admin surface: %d", intruded)
	}

	// The platform running something of its own in the workspace -- a check
	// suite, a git question -- does not take the running app's database away
	// from it. It used to: each one rotated the password the app had started
	// with, and every page that read data answered 500 until a restart.
	serving, err := db.store.GetWorkspace(ctx, installation.WorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	for i := 0; i < 2; i++ {
		if res, err := installer.deps.Workspaces.Run(ctx, serving, "true", nil, time.Minute); err != nil || !res.OK() {
			t.Fatalf("running a command in the workspace: %v", err)
		}
	}
	// A rotated password only refuses new connections, and the app's pool
	// had one open, which is why this went unseen until the pool recycled it
	// an hour later. Close the app's connections so the next page must
	// reconnect with what it was started with.
	var closed int
	terminate := fmt.Sprintf(`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = '%s'`,
		database.RoleFor(installation.Database))
	if err := db.QueryRowInt(ctx, installation.Database, terminate, &closed); err != nil {
		t.Fatalf("closing the app's connections: %v", err)
	}
	if closed == 0 {
		t.Fatal("the app held no connection to close, so this proves nothing")
	}
	// The pool checks a connection before reusing it once it has sat for a
	// second, which is what notices the closed ones.
	time.Sleep(1500 * time.Millisecond)

	// The public form is filled in by somebody with no account at all.
	visiting.be(nil)

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

	// The same site, a second form, a different audience: through the real
	// edge and the real app, each decided by the app's own rule.
	visiting.be(owner)
	if got := post(t, client, edge, base, "/forms", url.Values{
		"title": {"Team"}, "fields": {"Note"}, "audience": {"internal"},
	}, nil); got != http.StatusSeeOther {
		t.Fatalf("creating an internal form returned %d", got)
	}
	for _, c := range []struct {
		name string
		who  *domain.User
		want int
	}{{"nobody", nil, http.StatusUnauthorized}, {"a stranger", stranger, http.StatusForbidden}, {"a member", owner, http.StatusOK}} {
		visiting.be(c.who)
		if got := post(t, client, edge, base, "/f/team", url.Values{"note": {"hello"}}, nil); got != c.want {
			t.Errorf("%s answering an internal form got %d, want %d", c.name, got, c.want)
		}
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
	installer, _, owner, _, _ := newInstaller(t, dsn)

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

func newInstaller(t *testing.T, clusterDSN string) (*Installer, *tracked, *domain.User, string, *visiting) {
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
	// The edge asserts identity as well as stripping it, so the test can be
	// an anonymous visitor or a signed-in one by saying which.
	visiting := &visiting{}
	router := hostmux.New("test.invalid", visiting, quiet)
	edge := httptest.NewServer(router)
	t.Cleanup(edge.Close)

	// A port range of this test's own. Packages run in parallel and each has
	// its own store, so every allocator would otherwise start from the same
	// number and hand two packages the same port -- which only shows up later,
	// as a service that cannot restart because something else took its port.
	base := freeTestPort(t)
	allocator, err := preview.New(db, router, preview.Config{
		Domain: "test.invalid", Scheme: "http",
		PortStart: base, PortEnd: base + 200,
	})
	if err != nil {
		t.Fatalf("preview.New: %v", err)
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

	workspaces, err := workspace.New(workspace.Deps{
		Store: db, Driver: driver, Preview: allocator, Vault: vault,
		Overrides: NewData(db, databases),
	}, workspace.Options{
		Resources: vm.Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10},
		Image:     "dabberz-golden",
		Logger:    quiet,
	})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	// Registered after the store and databases, so it runs before they close.
	t.Cleanup(func() { releaseEverything(t, db, workspaces, dataDir) })

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
	return installer, databases, owner, edge.URL, visiting
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

// freeTestPort returns a port nothing is listening on, used as the base of
// this package's own preview range.
func freeTestPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// visiting is the edge's view of who is asking: whoever the test last said,
// or nobody. It stands in for a session established on an installation's
// hostname, which is the part of the identity story the platform owns.
type visiting struct {
	mu sync.Mutex
	as *domain.User
}

func (v *visiting) Visitor(context.Context, *http.Request) (*domain.User, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.as == nil {
		return nil, auth.ErrNoCredentials
	}
	return v.as, nil
}

// be makes the next requests through the edge come from this person, or from
// nobody when passed nil.
func (v *visiting) be(u *domain.User) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.as = u
}
