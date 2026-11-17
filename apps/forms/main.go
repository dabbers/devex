// Command forms is the Forms app for the dabberz platform.
//
// It is deliberately small. Its job is to prove the platform contract in
// docs/PLATFORM.md end to end, so that the fields the manifest is missing are
// discovered on a form rather than on somebody's CRM.
//
// Everything it needs arrives in the environment, provisioned by the platform:
// a database, an identity, a mail transport, a bucket. It holds no credentials
// of its own and reaches for no service the contract does not offer.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// version is set at build time.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		fmt.Fprintf(os.Stderr, "usage: forms serve\n")
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forms:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	if !cfg.identityReady() {
		// Fail-closed and say so at boot. The admin surface refuses to serve
		// without a way to tell who is asking; discovering that from a blank
		// page later is worse than refusing to start.
		logger.Warn("no identity is configured: the admin surface will refuse every request",
			"fix", "the platform sets "+trustEnv+"=1, or set "+devUserEnv+" to run this locally")
	}

	db, err := sql.Open("pgx", cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	// A web app's concurrency is bounded by the pool, not the other way round:
	// an unbounded pool turns a traffic spike into a database outage.
	db.SetMaxOpenConns(cfg.maxConns)
	db.SetMaxIdleConns(cfg.maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := &Store{db: db}
	if err := waitForDatabase(ctx, store, logger); err != nil {
		return err
	}
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	app, err := newApp(store, cfg, logger)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.port),
		Handler:           app.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	logger.Info("forms listening", "addr", server.Addr, "version", version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// waitForDatabase gives the platform a moment to finish provisioning.
//
// Services and their database come up together, so a first connection that
// fails is usually "not yet" rather than "never". Crashing immediately turns
// an ordinary race into a restart loop.
func waitForDatabase(ctx context.Context, store *Store, logger *slog.Logger) error {
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		err := store.Ready(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("database never became reachable: %w", err)
		}
		logger.Info("waiting for the database", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Environment variables the platform sets, named here so there is one list.
const (
	// trustEnv says this app is reachable only through the platform, which is
	// what makes the identity headers below trustworthy. The name is the
	// platform's, not this app's: an app that invents its own can be started
	// without it by accident and fall open.
	trustEnv = "DABBERZ_TRUSTED_PROXY"
	// devUserEnv runs the admin surface as a fixed person, for local work.
	devUserEnv = "FORMS_DEV_USER"
)

type config struct {
	databaseURL string
	port        int
	maxConns    int

	trustProxy bool
	devUser    string

	// Declared in the manifest and injected by the platform. Unset is a
	// working configuration, not an error: a form without spam protection
	// still collects answers.
	turnstileSecret string
}

func configFromEnv() (config, error) {
	cfg := config{
		databaseURL:     os.Getenv("DATABASE_URL"),
		port:            3000,
		maxConns:        10,
		trustProxy:      os.Getenv(trustEnv) == "1",
		devUser:         os.Getenv(devUserEnv),
		turnstileSecret: os.Getenv("TURNSTILE_SECRET"),
	}
	if cfg.databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is unset; the platform provisions it")
	}
	if raw := os.Getenv("PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port > 65535 {
			return config{}, fmt.Errorf("PORT=%q is not a port", raw)
		}
		cfg.port = port
	}
	if raw := os.Getenv("DATABASE_MAX_CONNECTIONS"); raw != "" {
		conns, err := strconv.Atoi(raw)
		if err != nil || conns <= 0 {
			return config{}, fmt.Errorf("DATABASE_MAX_CONNECTIONS=%q is not a count", raw)
		}
		cfg.maxConns = conns
	}
	return cfg, nil
}

// identityReady reports whether anyone can reach the admin surface.
func (c config) identityReady() bool { return c.trustProxy || c.devUser != "" }
