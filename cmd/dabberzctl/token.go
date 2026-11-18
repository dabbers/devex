package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/store"
)

// Issuing a credential for things that are not browsers.
//
// Deliberately not an API call. Every other command here talks to the control
// plane over HTTP, and this one opens the control plane's own database on
// disk, because an endpoint that mints credentials is a way in and this is
// not meant to be one. Minting requires being able to read the daemon's data
// directory, which means being root on the host, which already means being
// able to read the vault and every row in it. So this grants nothing new; it
// makes what root already has reachable from a script.
//
// It is also why there is no `token create --user`: this issues for the owner
// of the deployment, and anything more needs a real account system in front of
// it rather than filesystem access standing in for one.

func tokens(out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("token: say what to do: create, list, revoke")
	}
	switch args[0] {
	case "create":
		return tokenCreate(out, args[1:])
	case "list":
		return tokenList(out, args[1:])
	case "revoke":
		return tokenRevoke(out, args[1:])
	default:
		return fmt.Errorf("token: %q is not create, list or revoke", args[0])
	}
}

// openStore reaches the control plane's database directly.
func openStore(ctx context.Context, dataDir string) (*store.Store, error) {
	path := filepath.Join(dataDir, "dabberz.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("token: cannot read %s: %w\n"+
			"Run this on the host, as root", path, err)
	}
	return store.Open(ctx, path)
}

func tokenFlags(args []string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	// Read from the config rather than defaulted to a path, which would be
	// one deployment's layout baked into a tool every deployment uses.
	cfgPath := fs.String("config", defaultConfigPath,
		"the daemon's config, read for data_dir")
	owner := fs.String("owner", "", "the owner's email; defaults to the only account")
	return fs, cfgPath, owner
}

// dataDirFrom reads where the control plane keeps its database.
//
// One field, parsed directly, rather than config.Load. Loading validates the
// whole configuration, which would make issuing a token require the master
// key and the provider's secret -- neither of which it uses. Those live in
// systemd's environment file rather than in an operator's shell, so the
// command would fail for the one person it exists to serve, with a message
// about a key it does not need.
func dataDirFrom(cfgPath string) (string, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", fmt.Errorf("token: cannot read %s: %w\n"+
			"Run this on the host, as root, or say where with -config", cfgPath, err)
	}
	var partial struct {
		DataDir string `yaml:"data_dir"`
	}
	if err := yaml.Unmarshal(raw, &partial); err != nil {
		return "", fmt.Errorf("token: %s is not readable as yaml: %w", cfgPath, err)
	}
	if partial.DataDir == "" {
		return "", fmt.Errorf("token: %s sets no data_dir", cfgPath)
	}
	return partial.DataDir, nil
}

func tokenCreate(out io.Writer, args []string) error {
	fs, cfgPath, owner := tokenFlags(args)
	name := fs.String("name", "", "what this token is for")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	dataDir, err := dataDirFrom(*cfgPath)
	if err != nil {
		return err
	}
	db, err := openStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	user, err := resolveOwner(ctx, db, *owner)
	if err != nil {
		return err
	}

	label := *name
	if label == "" {
		host, _ := os.Hostname()
		label = "cli on " + host
	}

	_, plain, err := db.CreateToken(ctx, user.ID, label)
	if err != nil {
		return err
	}

	// Printed once, because it is not stored. Said plainly rather than left
	// for somebody to discover by trying to look it up again.
	fmt.Fprintf(out, "%s\n\nThis is shown once and is not recoverable. Use it with:\n"+
		"  export DABBERZ_TOKEN=%s\n\nIt authenticates as %s.\n", plain, plain, user.Email)
	return nil
}

func tokenList(out io.Writer, args []string) error {
	fs, cfgPath, owner := tokenFlags(args)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	dataDir, err := dataDirFrom(*cfgPath)
	if err != nil {
		return err
	}
	db, err := openStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	user, err := resolveOwner(ctx, db, *owner)
	if err != nil {
		return err
	}
	issued, err := db.ListTokens(ctx, user.ID)
	if err != nil {
		return err
	}
	if len(issued) == 0 {
		fmt.Fprintln(out, "no tokens")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCREATED\tLAST USED")
	for _, t := range issued {
		used := "never"
		if t.LastUsedAt != nil {
			used = t.LastUsedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, t.Name, t.CreatedAt.Format(time.RFC3339), used)
	}
	return w.Flush()
}

func tokenRevoke(out io.Writer, args []string) error {
	fs, cfgPath, owner := tokenFlags(args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("token revoke: say which token (see `dabberzctl token list`)")
	}

	ctx := context.Background()
	dataDir, err := dataDirFrom(*cfgPath)
	if err != nil {
		return err
	}
	db, err := openStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	user, err := resolveOwner(ctx, db, *owner)
	if err != nil {
		return err
	}
	for _, tokenID := range fs.Args() {
		if err := db.RevokeToken(ctx, user.ID, tokenID); err != nil {
			return err
		}
		fmt.Fprintln(out, "revoked", tokenID)
	}
	return nil
}

// resolveOwner finds the account to issue for.
//
// By email when given. Otherwise the single user, and only when there is
// exactly one: guessing which of several accounts a token should authenticate
// as is not a guess worth making silently.
func resolveOwner(ctx context.Context, db *store.Store, email string) (*domain.User, error) {
	if email != "" {
		user, err := db.GetUserByEmail(ctx, email)
		if err != nil {
			return nil, fmt.Errorf("token: no account for %s: %w", email, err)
		}
		return user, nil
	}

	users, err := db.ListUsers(ctx, 2)
	if err != nil {
		return nil, err
	}
	switch len(users) {
	case 0:
		return nil, fmt.Errorf("token: this deployment has no accounts yet; " +
			"sign in once so the owner row exists")
	case 1:
		return users[0], nil
	default:
		return nil, fmt.Errorf("token: more than one account here; say which with -owner")
	}
}
