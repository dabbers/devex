// Package database provisions and protects the Postgres an app runs on.
//
// This is the platform's data block. An app declares that it needs a database
// and the platform provides one: created, credentialed, isolated from every
// other installation's, snapshotted before anything risky, and restorable.
// The app owns its schema and its queries and nothing else.
//
// Two properties matter more than the rest, because they are the ones that
// cost a customer their business when they are wrong.
//
// One installation cannot read another's data. Every installation gets its own
// database and its own role, the role is granted nothing outside that database,
// and the default public privileges that would otherwise let any role connect
// are revoked. This is the data half of the tenancy boundary the API already
// enforces on routes.
//
// A restore has to actually work. Snapshots that have never been restored are
// a belief rather than a backup, so the restore path here is the same one the
// tests exercise, against a real cluster, with real data in it.
package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	// Registers the "pgx" driver. The platform provisions the Postgres its
	// apps run on, so it needs a client of its own.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ErrNotFound reports that a database or snapshot is not there.
var ErrNotFound = errors.New("database: not found")

// Database is one installation's store.
type Database struct {
	// Name is the Postgres database. Generated, never supplied: an
	// identifier cannot be parameterised in DDL, so the only safe policy is
	// that nothing a caller controls ever reaches a CREATE statement.
	Name string `json:"name"`
	// Role is the login the app connects as. It owns this database and has
	// rights nowhere else.
	Role string `json:"role"`
	// Owner is the platform's id for whoever this belongs to, recorded as a
	// comment on the database so the cluster can be read without the control
	// plane's own records.
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
}

// Snapshot is a point a database can be returned to.
type Snapshot struct {
	ID       string    `json:"id"`
	Database string    `json:"database"`
	Label    string    `json:"label"`
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	TakenAt  time.Time `json:"taken_at"`
}

// Config configures the block.
type Config struct {
	// AdminDSN reaches the cluster as a role that may create databases and
	// roles. It never leaves this package.
	AdminDSN string
	// PublicHost is what apps are told to connect to, when that differs from
	// the address the control plane administers through.
	PublicHost string
	// SnapshotDir holds dumps. It must be on storage that outlives the
	// machine: a snapshot on the same disk as the database it protects is not
	// a backup, it is a copy.
	SnapshotDir string
	// BinDir locates pg_dump and pg_restore. Empty uses PATH.
	BinDir string
	// Timeout bounds one dump or restore.
	Timeout time.Duration
}

// DefaultTimeout bounds a dump or restore that named none.
const DefaultTimeout = 30 * time.Minute

// Block provisions databases on one Postgres cluster.
type Block struct {
	admin *sql.DB
	cfg   Config
}

// Open connects to the cluster.
func Open(cfg Config) (*Block, error) {
	if strings.TrimSpace(cfg.AdminDSN) == "" {
		return nil, errors.New("database: an admin connection string is required")
	}
	if strings.TrimSpace(cfg.SnapshotDir) == "" {
		return nil, errors.New("database: a snapshot directory is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if err := os.MkdirAll(cfg.SnapshotDir, 0o700); err != nil {
		return nil, fmt.Errorf("database: snapshot directory: %w", err)
	}

	admin, err := sql.Open("pgx", cfg.AdminDSN)
	if err != nil {
		return nil, fmt.Errorf("database: open cluster: %w", err)
	}
	// Administration is occasional and serial. A pool here would hold
	// connections open against a cluster whose connection count is a real
	// limit shared with every app on it.
	admin.SetMaxOpenConns(2)
	admin.SetConnMaxIdleTime(time.Minute)
	return &Block{admin: admin, cfg: cfg}, nil
}

// Close releases the administrative connection.
func (b *Block) Close() error { return b.admin.Close() }

// Ping reports whether the cluster is reachable.
func (b *Block) Ping(ctx context.Context) error { return b.admin.PingContext(ctx) }

// Provision creates a database and the role that owns it, and returns the
// connection string the app is given.
//
// The DSN is returned once and never stored here. It is the platform's to hand
// to the app as an environment variable; keeping a copy would make this package
// a place worth stealing.
func (b *Block) Provision(ctx context.Context, owner string) (*Database, string, error) {
	name := "app_" + randomName()
	db := &Database{
		Name:      name,
		Role:      RoleFor(name),
		Owner:     owner,
		CreatedAt: time.Now().UTC(),
	}
	password, err := randomSecret()
	if err != nil {
		return nil, "", err
	}

	// Role first: a database is created owned by it, which is what makes the
	// app able to create its own tables without any rights outside.
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD %s`, quoteID(db.Role), quoteLiteral(password))); err != nil {
		return nil, "", fmt.Errorf("database: create role: %w", err)
	}
	// CREATE DATABASE cannot run inside a transaction, so a failure here has
	// to clean up the role by hand rather than rolling back.
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE DATABASE %s OWNER %s`, quoteID(db.Name), quoteID(db.Role))); err != nil {
		_, _ = b.admin.ExecContext(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, quoteID(db.Role)))
		return nil, "", fmt.Errorf("database: create database: %w", err)
	}

	if err := b.isolate(ctx, db, owner); err != nil {
		_ = b.Drop(ctx, db)
		return nil, "", err
	}
	return db, b.dsn(db.Name, db.Role, password), nil
}

// isolate removes the privileges Postgres grants by default.
//
// Out of the box every role may connect to every database and create objects
// in its public schema. For a single-tenant cluster that is a convenience; for
// this one it means any installation could read any other's data, so it is
// revoked rather than relied upon not to matter.
func (b *Block) isolate(ctx context.Context, db *Database, owner string) error {
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`REVOKE ALL ON DATABASE %s FROM PUBLIC`, quoteID(db.Name))); err != nil {
		return fmt.Errorf("database: revoke public access: %w", err)
	}
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`GRANT ALL ON DATABASE %s TO %s`, quoteID(db.Name), quoteID(db.Role))); err != nil {
		return fmt.Errorf("database: grant owner: %w", err)
	}
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`COMMENT ON DATABASE %s IS %s`,
		quoteID(db.Name), quoteLiteral("dabberz installation "+owner))); err != nil {
		return fmt.Errorf("database: label database: %w", err)
	}

	// The public schema's default CREATE grant is a property of the database,
	// so it has to be revoked from inside it.
	inside, err := sql.Open("pgx", b.adminDSNFor(db.Name))
	if err != nil {
		return err
	}
	defer inside.Close()
	if _, err := inside.ExecContext(ctx, `REVOKE ALL ON SCHEMA public FROM PUBLIC`); err != nil {
		return fmt.Errorf("database: revoke public schema: %w", err)
	}
	if _, err := inside.ExecContext(ctx, fmt.Sprintf(
		`GRANT ALL ON SCHEMA public TO %s`, quoteID(db.Role))); err != nil {
		return fmt.Errorf("database: grant schema: %w", err)
	}
	return nil
}

// Drop removes a database and its role. Irreversible, and used when an
// installation is deleted after its export has been taken.
func (b *Block) Drop(ctx context.Context, db *Database) error {
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`DROP DATABASE IF EXISTS %s WITH (FORCE)`, quoteID(db.Name))); err != nil {
		return fmt.Errorf("database: drop database: %w", err)
	}
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`DROP ROLE IF EXISTS %s`, quoteID(db.Role))); err != nil {
		return fmt.Errorf("database: drop role: %w", err)
	}
	return nil
}

// Snapshot captures a database so it can be returned to.
//
// Taken before anything that changes data irreversibly -- a migration, an
// import, a promote carrying either -- which is the platform's job rather than
// the app's, because an app cannot be trusted to remember and a customer
// should not have to.
func (b *Block) Snapshot(ctx context.Context, db *Database, label string) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	snapshot := &Snapshot{
		ID:       "snp_" + randomName(),
		Database: db.Name,
		Label:    strings.TrimSpace(label),
		TakenAt:  time.Now().UTC(),
	}
	snapshot.Path = filepath.Join(b.cfg.SnapshotDir, snapshot.ID+".dump")

	// Custom format, because it restores selectively and compresses. A plain
	// SQL dump would need the whole file replayed to recover one table.
	cmd := exec.CommandContext(ctx, b.bin("pg_dump"),
		"--format=custom", "--no-owner", "--no-privileges",
		"--file="+snapshot.Path, b.adminDSNFor(db.Name))
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(snapshot.Path)
		return nil, fmt.Errorf("database: snapshot %s: %w: %s", db.Name, err, trim(output))
	}

	info, err := os.Stat(snapshot.Path)
	if err != nil {
		return nil, fmt.Errorf("database: snapshot %s produced no file: %w", db.Name, err)
	}
	snapshot.Bytes = info.Size()
	return snapshot, nil
}

// Restore returns a database to a snapshot.
//
// This discards everything written since the snapshot was taken. Code and data
// roll back differently and conflating them is how a rollback button loses a
// day of submissions, so this is deliberately a separate, explicit operation
// from redeploying a previous build.
func (b *Block) Restore(ctx context.Context, db *Database, snapshot *Snapshot) error {
	if _, err := os.Stat(snapshot.Path); err != nil {
		return fmt.Errorf("database: %w: snapshot %s", ErrNotFound, snapshot.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	// An empty database rather than a merge: restoring over existing rows
	// would leave a mixture of two points in time, which is not a restore and
	// is worse than either.
	if err := b.recreate(ctx, db); err != nil {
		return err
	}

	// --role makes pg_restore SET ROLE before it creates anything, so every
	// table comes out owned by the app rather than by the administrator that
	// ran the restore. The alternative -- restore as admin and hand ownership
	// over afterwards -- cannot work when the administrator is the cluster's
	// bootstrap role, because reassigning its objects is refused.
	cmd := exec.CommandContext(ctx, b.bin("pg_restore"),
		"--no-owner", "--no-privileges", "--exit-on-error",
		"--role="+db.Role,
		"--dbname="+b.adminDSNFor(db.Name), snapshot.Path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("database: restore %s into %s: %w: %s",
			snapshot.ID, db.Name, err, trim(output))
	}
	return nil
}

// recreate empties a database by dropping and remaking it, which is faster and
// more complete than deleting from every table and cannot leave a stray type
// or sequence behind.
func (b *Block) recreate(ctx context.Context, db *Database) error {
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`DROP DATABASE IF EXISTS %s WITH (FORCE)`, quoteID(db.Name))); err != nil {
		return fmt.Errorf("database: clear %s: %w", db.Name, err)
	}
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE DATABASE %s OWNER %s`, quoteID(db.Name), quoteID(db.Role))); err != nil {
		return fmt.Errorf("database: recreate %s: %w", db.Name, err)
	}
	return b.isolate(ctx, db, db.Owner)
}

// ListSnapshots returns what has been captured for a database, newest first.
func (b *Block) ListSnapshots(_ context.Context, db *Database) ([]*Snapshot, error) {
	entries, err := os.ReadDir(b.cfg.SnapshotDir)
	if err != nil {
		return nil, fmt.Errorf("database: list snapshots: %w", err)
	}

	snapshots := make([]*Snapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dump") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		snapshots = append(snapshots, &Snapshot{
			ID:       strings.TrimSuffix(entry.Name(), ".dump"),
			Database: db.Name,
			Path:     filepath.Join(b.cfg.SnapshotDir, entry.Name()),
			Bytes:    info.Size(),
			TakenAt:  info.ModTime().UTC(),
		})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].TakenAt.After(snapshots[j].TakenAt)
	})
	return snapshots, nil
}

// --------------------------------------------------------------- plumbing

func (b *Block) bin(name string) string {
	if b.cfg.BinDir == "" {
		return name
	}
	return filepath.Join(b.cfg.BinDir, name)
}

// dsn builds the connection string an app is handed.
func (b *Block) dsn(database, role, password string) string {
	host := b.cfg.PublicHost
	if host == "" {
		host = hostOf(b.cfg.AdminDSN)
	}
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     host,
		Path:     "/" + database,
		RawQuery: "sslmode=" + sslModeOf(b.cfg.AdminDSN),
	}).String()
}

// adminDSNFor points the administrative credentials at one database.
func (b *Block) adminDSNFor(database string) string {
	parsed, err := url.Parse(b.cfg.AdminDSN)
	if err != nil {
		return b.cfg.AdminDSN
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func hostOf(dsn string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "127.0.0.1:5432"
}

func sslModeOf(dsn string) string {
	if parsed, err := url.Parse(dsn); err == nil {
		if mode := parsed.Query().Get("sslmode"); mode != "" {
			return mode
		}
	}
	return "require"
}

// identifierPattern is what a generated name must look like. Nothing a caller
// supplies ever becomes an identifier, and this asserts that rather than
// trusting it.
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}$`)

// quoteID quotes a generated identifier for DDL, where placeholders do not
// exist. It panics on anything that is not already safe, because a name that
// failed this check came from somewhere it should not have.
func quoteID(name string) string {
	if !identifierPattern.MatchString(name) {
		panic("database: refusing to build DDL with the identifier " + name)
	}
	return `"` + name + `"`
}

// quoteLiteral quotes a string for a statement that cannot take a parameter,
// which for this package is only ever a generated password or a label.
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

var nameAlphabet = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

// randomName returns a lowercase identifier fragment.
func randomName() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("database: no randomness available: " + err.Error())
	}
	return nameAlphabet.EncodeToString(raw[:])
}

// randomSecret returns a password for a role.
func randomSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("database: no randomness available: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]), nil
}

func trim(output []byte) string {
	text := strings.TrimSpace(string(output))
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return text
}

// Connect opens a connection to one provisioned database using the
// administrative credentials.
//
// For the platform's own work -- a snapshot, an inspection, a restore -- never
// for serving an app: an app connects with the role it was given, which is
// what keeps one installation out of another's data.
func (b *Block) Connect(name string) (*sql.DB, error) {
	if !identifierPattern.MatchString(name) {
		return nil, fmt.Errorf("database: %q is not a database this package created", name)
	}
	return sql.Open("pgx", b.adminDSNFor(name))
}

// Credentials rotates a database's password and returns a connection string.
//
// Called when a workspace starts rather than remembered from when the database
// was created, which means no DSN is ever written down -- not in the vault,
// not in a row, not in a log. A credential that is never stored cannot be
// taken from storage, and the platform owns the role, so minting a new one
// costs nothing.
//
// It invalidates whatever was issued before. That is the point: the only thing
// holding the old one is a process that is being replaced.
func (b *Block) Credentials(ctx context.Context, db *Database) (string, error) {
	password, err := randomSecret()
	if err != nil {
		return "", err
	}
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(
		`ALTER ROLE %s WITH PASSWORD %s`, quoteID(db.Role), quoteLiteral(password))); err != nil {
		return "", fmt.Errorf("database: rotate credentials for %s: %w", db.Name, err)
	}
	return b.dsn(db.Name, db.Role, password), nil
}

// RoleFor returns the role name belonging to a database this package created.
//
// Derived rather than remembered, so a caller holding only a database name --
// which is all the control plane records -- can still reach it.
func RoleFor(databaseName string) string { return databaseName + "_rw" }
