package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// A real cluster, deliberately. What this package does is almost entirely
// privilege grants and process invocation, neither of which exists in a fake:
// a stubbed Postgres would prove the stub isolates tenants, which is not the
// question.
const clusterEnv = "DABBERZ_TEST_CLUSTER_DSN"

func testBlock(t *testing.T) *Block {
	t.Helper()

	dsn := os.Getenv(clusterEnv)
	if dsn == "" {
		t.Skip("set " + clusterEnv + " to a superuser DSN to run against a real cluster")
	}
	block, err := Open(Config{
		AdminDSN:    unprivileged(t, dsn),
		SnapshotDir: t.TempDir(),
		BinDir:      os.Getenv("DABBERZ_TEST_PG_BIN"),
		Timeout:     2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { block.Close() })
	return block
}

// unprivileged returns a DSN for a fresh role shaped like the one a real host
// runs as: CREATEROLE and CREATEDB, not superuser.
//
// The superuser the tests are handed is not what production has, and it
// passes checks production fails. Every test here once passed against one
// while the first install on dbz-alpha was refused outright, because since
// Postgres 16 creating a role no longer makes its creator able to act as it.
func unprivileged(t *testing.T, superDSN string) string {
	t.Helper()

	super, err := sql.Open("pgx", superDSN)
	if err != nil {
		t.Fatalf("open superuser: %v", err)
	}
	defer super.Close()

	role := "platform_test_" + randomName()
	password, err := randomSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(fmt.Sprintf(
		`CREATE ROLE %s LOGIN CREATEROLE CREATEDB NOSUPERUSER PASSWORD %s`,
		quoteID(role), quoteLiteral(password))); err != nil {
		t.Fatalf("create unprivileged role: %v", err)
	}
	// Registered first, so it runs last: after every database the role made,
	// and its block's connections, are gone.
	t.Cleanup(func() {
		super, err := sql.Open("pgx", superDSN)
		if err != nil {
			t.Errorf("reopen superuser: %v", err)
			return
		}
		defer super.Close()
		if _, err := super.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, quoteID(role))); err != nil {
			t.Errorf("drop %s: %v", role, err)
		}
	})

	u, err := url.Parse(superDSN)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", clusterEnv, err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

// provision makes a database and guarantees it is cleaned up, so a failing
// test does not leave roles behind on a shared cluster.
func provision(t *testing.T, block *Block, owner string) (*Database, string) {
	t.Helper()

	db, dsn, err := block.Provision(context.Background(), owner)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := block.Drop(context.Background(), db); err != nil {
			t.Errorf("Drop(%s): %v", db.Name, err)
		}
	})
	return db, dsn
}

// openAs connects with the credentials an app would be handed.
func openAs(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestAProvisionedDatabaseIsUsable(t *testing.T) {
	block := testBlock(t)
	db, dsn := provision(t, block, "ins_1")
	ctx := context.Background()

	conn := openAs(t, dsn)
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("the app could not connect with what it was given: %v", err)
	}
	// An app owns its schema: it has to be able to create tables without
	// asking the platform for anything.
	if _, err := conn.ExecContext(ctx,
		`CREATE TABLE things (id int primary key, note text)`); err != nil {
		t.Fatalf("the app could not create its own schema: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO things VALUES (1, 'hello')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var note string
	if err := conn.QueryRowContext(ctx, `SELECT note FROM things WHERE id = 1`).Scan(&note); err != nil {
		t.Fatalf("select: %v", err)
	}
	if note != "hello" {
		t.Fatalf("note = %q", note)
	}
	if db.Owner != "ins_1" {
		t.Fatalf("owner = %q", db.Owner)
	}
}

// TestOneInstallationCannotReachAnother is the data half of the tenancy
// boundary. Postgres grants every role the right to connect to every database
// by default, so this is a property that has to be taken away rather than one
// that is there to begin with -- which means it is worth a test that would
// notice if the revoke were ever dropped.
func TestOneInstallationCannotReachAnother(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()

	victim, victimDSN := provision(t, block, "ins_victim")
	_, intruderDSN := provision(t, block, "ins_intruder")

	// The victim stores something worth taking.
	victimConn := openAs(t, victimDSN)
	if _, err := victimConn.ExecContext(ctx,
		`CREATE TABLE secrets (value text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := victimConn.ExecContext(ctx,
		`INSERT INTO secrets VALUES ('the customer list')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The intruder holds valid credentials for their own database. Pointing
	// them at somebody else's must fail at connect.
	crossDSN := strings.Replace(intruderDSN,
		pathOf(t, intruderDSN), "/"+victim.Name, 1)
	cross, err := sql.Open("pgx", crossDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cross.Close()

	var stolen string
	err = cross.QueryRowContext(ctx, `SELECT value FROM secrets`).Scan(&stolen)
	if err == nil {
		t.Fatalf("one installation read another's data: %q", stolen)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Logf("refused with: %v", err)
	}
}

// TestARestoreActuallyRestores is the drill. A snapshot that has never been
// restored is a belief, not a backup, so the path a customer would depend on
// in their worst hour is the one exercised here, with real data in it.
func TestARestoreActuallyRestores(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()
	db, dsn := provision(t, block, "ins_restore")

	conn := openAs(t, dsn)
	if _, err := conn.ExecContext(ctx,
		`CREATE TABLE submissions (id serial primary key, body text not null)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, body := range []string{"first", "second", "third"} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO submissions (body) VALUES ($1)`, body); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	snapshot, err := block.Snapshot(ctx, db, "before the risky migration")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Bytes == 0 {
		t.Fatal("the snapshot file is empty")
	}

	// Something goes wrong: a migration drops the table.
	if _, err := conn.ExecContext(ctx, `DROP TABLE submissions`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	if err := block.Restore(ctx, db, snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The app's own credentials still work afterwards. A restore that leaves
	// the database owned by the administrator is a restore the app cannot use.
	restored := openAs(t, dsn)
	var count int
	if err := restored.QueryRowContext(ctx, `SELECT count(*) FROM submissions`).Scan(&count); err != nil {
		t.Fatalf("the app cannot read its restored data: %v", err)
	}
	if count != 3 {
		t.Fatalf("restored %d rows, want 3", count)
	}
	// And it can still write, which needs ownership rather than read access.
	if _, err := restored.ExecContext(ctx,
		`INSERT INTO submissions (body) VALUES ('after the restore')`); err != nil {
		t.Fatalf("the app cannot write after a restore: %v", err)
	}
}

// TestARestoreDiscardsWhatCameAfterIt: code and data roll back differently and
// the interface has to say so. This pins the behaviour the warning describes.
func TestARestoreDiscardsWhatCameAfterIt(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()
	db, dsn := provision(t, block, "ins_discard")

	conn := openAs(t, dsn)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE rows (body text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO rows VALUES ('before')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	snapshot, err := block.Snapshot(ctx, db, "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO rows VALUES ('after')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := block.Restore(ctx, db, snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored := openAs(t, dsn)
	var bodies []string
	results, err := restored.QueryContext(ctx, `SELECT body FROM rows`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer results.Close()
	for results.Next() {
		var body string
		if err := results.Scan(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	if len(bodies) != 1 || bodies[0] != "before" {
		t.Fatalf("rows after restore = %v, want only what existed at the snapshot", bodies)
	}
}

func TestSnapshotsAreListedNewestFirst(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()
	db, dsn := provision(t, block, "ins_list")

	conn := openAs(t, dsn)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE t (a int)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for range 2 {
		if _, err := block.Snapshot(ctx, db, "routine"); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		time.Sleep(1100 * time.Millisecond) // file timestamps have second resolution
	}

	snapshots, err := block.ListSnapshots(ctx, db)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snapshots))
	}
	if snapshots[0].TakenAt.Before(snapshots[1].TakenAt) {
		t.Fatal("snapshots are not newest first")
	}
}

func TestRestoringAMissingSnapshotIsRefused(t *testing.T) {
	block := testBlock(t)
	db, _ := provision(t, block, "ins_missing")

	err := block.Restore(context.Background(), db, &Snapshot{
		ID: "snp_gone", Database: db.Name, Path: filepath.Join(t.TempDir(), "absent.dump"),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDropRemovesTheRoleAsWellAsTheDatabase(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()

	db, dsn, err := block.Provision(ctx, "ins_dropped")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := block.Drop(ctx, db); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	// A role left behind after its database is gone accumulates on the
	// cluster and keeps working as a credential.
	var exists bool
	if err := block.admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, db.Role).Scan(&exists); err != nil {
		t.Fatalf("check role: %v", err)
	}
	if exists {
		t.Fatalf("the role %s outlived its database", db.Role)
	}

	conn, err := sql.Open("pgx", dsn)
	if err == nil {
		defer conn.Close()
		if conn.PingContext(ctx) == nil {
			t.Fatal("the dropped credentials still connect")
		}
	}
}

// TestGeneratedIdentifiersAreTheOnlyOnesUsed guards the rule that keeps DDL
// safe: Postgres cannot parameterise an identifier, so nothing a caller
// controls may ever reach a CREATE statement.
func TestGeneratedIdentifiersAreTheOnlyOnesUsed(t *testing.T) {
	t.Parallel()

	for _, hostile := range []string{
		`app"; DROP DATABASE postgres; --`,
		"app' OR '1'='1",
		"APP_UPPER",
		"ab",
		"",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("quoteID(%q) built DDL instead of refusing", hostile)
				}
			}()
			_ = quoteID(hostile)
		}()
	}

	// What the package generates must pass its own check.
	for range 50 {
		_ = quoteID("app_" + randomName())
	}
}

// pathOf returns the database path of a DSN, so a test can repoint it.
func pathOf(t *testing.T, dsn string) string {
	t.Helper()
	at := strings.LastIndex(dsn, "@")
	rest := dsn[at+1:]
	slash := strings.Index(rest, "/")
	question := strings.Index(rest, "?")
	if question < 0 {
		question = len(rest)
	}
	return rest[slash:question]
}

// TestAForkIsARealCopy is the property a change depends on: its data is the
// installation's data, and nothing it does reaches back.
func TestAForkIsARealCopy(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()

	source, sourceDSN := provision(t, block, "ins_source")
	conn := openAs(t, sourceDSN)
	if _, err := conn.ExecContext(ctx,
		`CREATE TABLE submissions (id serial primary key, body text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO submissions (body) VALUES ('a real customer')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	forked, err := block.Fork(ctx, source, "chg_1")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() {
		if err := block.Drop(context.Background(), forked); err != nil {
			t.Logf("dropping %s: %v", forked.Name, err)
		}
	})
	if forked.Name == source.Name {
		t.Fatal("the fork is the source")
	}

	// It holds the real data: a change tested against an empty database has
	// not been tested.
	forkConn, err := block.Connect(forked.Name)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer forkConn.Close()

	var body string
	if err := forkConn.QueryRowContext(ctx, `SELECT body FROM submissions`).Scan(&body); err != nil {
		t.Fatalf("the fork does not hold the source's data: %v", err)
	}
	if body != "a real customer" {
		t.Fatalf("the fork holds %q", body)
	}

	// And nothing it does reaches back.
	if _, err := forkConn.ExecContext(ctx, `DELETE FROM submissions`); err != nil {
		t.Fatalf("delete in the fork: %v", err)
	}
	if _, err := forkConn.ExecContext(ctx,
		`INSERT INTO submissions (body) VALUES ('an experiment')`); err != nil {
		t.Fatalf("insert in the fork: %v", err)
	}

	var remaining string
	if err := conn.QueryRowContext(ctx, `SELECT body FROM submissions`).Scan(&remaining); err != nil {
		t.Fatalf("reading the source after the fork was changed: %v", err)
	}
	if remaining != "a real customer" {
		t.Fatalf("the source now holds %q; the fork leaked into it", remaining)
	}
}

// TestAForkLeavesNoSnapshotBehind: the dump a fork goes through is a byproduct,
// not a backup, and keeping one per change would fill the disk with copies
// nobody asked for.
func TestAForkLeavesNoSnapshotBehind(t *testing.T) {
	block := testBlock(t)
	ctx := context.Background()

	source, dsn := provision(t, block, "ins_tidy")
	conn := openAs(t, dsn)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE t (a int)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	before, err := block.ListSnapshots(ctx, source)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	forked, err := block.Fork(ctx, source, "chg_tidy")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { _ = block.Drop(context.Background(), forked) })

	after, err := block.ListSnapshots(ctx, source)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("forking left %d snapshots behind", len(after)-len(before))
	}
}
