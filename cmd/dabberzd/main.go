// Command dabberzd is the dabberz control plane.
//
// It owns the database, the scheduler, the VM driver and the web API, and runs
// on the single machine the fork VMs run on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/api"
	"github.com/dabbers/devex/internal/config"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/merge"
	"github.com/dabbers/devex/internal/orchestrator"
	"github.com/dabbers/devex/internal/pipeline"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/proxy"
	"github.com/dabbers/devex/internal/proxy/caddy"
	"github.com/dabbers/devex/internal/scheduler"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/firecracker"
	"github.com/dabbers/devex/internal/vm/local"
	"github.com/dabbers/devex/internal/web"
)

// version is set at build time.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dabberzd:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "configs/dabberz.yaml", "path to the configuration file")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("dabberzd", version)
		return nil
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	for _, warning := range cfg.Warnings() {
		logger.Warn(warning)
	}

	// Signals stop the whole tree: the scheduler, the HTTP server and any
	// in-flight pipeline work share this context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := build(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer app.close()

	return app.serve(ctx)
}

// application holds the wired control plane.
type application struct {
	cfg    config.Config
	logger *slog.Logger
	store  *store.Store
	sched  *scheduler.Scheduler
	server *http.Server
}

// build wires every component together.
func build(ctx context.Context, cfg config.Config, logger *slog.Logger) (*application, error) {
	dataDir := cfg.DataDir
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", dataDir, err)
	}

	db, err := store.Open(ctx, filepath.Join(dataDir, "dabberz.db"))
	if err != nil {
		return nil, err
	}
	// The database is closed here on any failure below; on success its
	// ownership passes to the application, which closes it on shutdown.
	wired := false
	defer func() {
		if wired {
			return
		}
		if err := db.Close(); err != nil {
			logger.Error("could not close the database after a failed startup", "error", err)
		}
	}()

	// v1 is single-user, but everything is scoped by this id so that
	// multi-user support is additive rather than a rewrite.
	owner, err := db.EnsureUser(ctx, cfg.Owner)
	if err != nil {
		return nil, err
	}

	masterKey, err := secrets.ParseKey(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	vault, err := secrets.NewVault(db, masterKey)
	if err != nil {
		return nil, err
	}

	mem, err := memory.New(filepath.Join(dataDir, "memory"))
	if err != nil {
		return nil, err
	}

	driver, err := buildDriver(cfg, dataDir)
	if err != nil {
		return nil, err
	}
	logger.Info("vm driver ready", "driver", driver.Name())

	router := buildRouter(cfg)
	allocator, err := preview.New(db, router, cfg.Preview)
	if err != nil {
		return nil, err
	}
	// Republish on startup so the proxy matches the database even if it was
	// restarted, reconfigured or edited by hand while dabberzd was down.
	if err := allocator.Publish(ctx); err != nil {
		logger.Warn("could not publish preview routes at startup", "error", err)
	}

	modelClient, err := buildModel(cfg, logger)
	if err != nil {
		return nil, err
	}

	// The scheduler is built first so the orchestrator can nudge it, then
	// given its launcher once the pipeline exists.
	var sched *scheduler.Scheduler
	orch := orchestrator.New(db, modelClient, mem, nudgeFunc(func() {
		if sched != nil {
			sched.Nudge()
		}
	}), orchestrator.Options{Tripwire: cfg.Tripwire, Logger: logger})

	agentRunner := agent.New(driver, cfg.Agent)

	verifier, err := buildVerifier(ctx, driver, &cfg, logger)
	if err != nil {
		return nil, err
	}

	var launcher scheduler.Launcher
	if verifier != nil {
		pipe, err := pipeline.New(pipeline.Deps{
			Store: db, Driver: driver, Agent: agentRunner, Verifier: verifier,
			Reviewer: merge.New(driver, agentRunner, cfg.Merge),
			Preview:  allocator, Vault: vault, Orch: orch,
		}, pipeline.Options{
			ForkResources: cfg.Driver.ForkResources,
			Image:         cfg.Driver.Image,
			Logger:        logger,
		})
		if err != nil {
			return nil, err
		}
		launcher = pipe
	} else {
		// Without a UI VM nothing can be verified, and nothing merges
		// unverified. Forks would stall rather than progress, so they are left
		// queued and the reason is said plainly.
		logger.Warn("no shared UI VM configured; forks will queue and not start")
	}

	sched = scheduler.New(db, driver, launcher, scheduler.Options{
		Interval:           cfg.Scheduler.Interval,
		ForkResources:      cfg.Driver.ForkResources,
		HeadOfLineBlocking: cfg.Scheduler.HeadOfLineBlocking,
		Logger:             logger,
	})

	ui, err := web.Handler()
	if err != nil {
		return nil, err
	}

	apiServer, err := api.New(api.Deps{
		Store: db, Orch: orch, Sched: sched, Preview: allocator,
		Vault: vault, Memory: mem, Verifier: verifier,
		UI: ui, Owner: owner, Logger: logger,
	})
	if err != nil {
		return nil, err
	}

	wired = true
	return &application{
		cfg:    cfg,
		logger: logger,
		store:  db,
		sched:  sched,
		server: &http.Server{
			Addr:        cfg.Server.Addr,
			Handler:     apiServer.Handler(),
			ReadTimeout: cfg.Server.ReadTimeout,
			// No write timeout: the activity stream is a long-lived response.
			WriteTimeout: cfg.Server.WriteTimeout,
			BaseContext:  func(net.Listener) context.Context { return ctx },
		},
	}, nil
}

// serve runs the scheduler and the HTTP server until the context ends.
func (a *application) serve(ctx context.Context) error {
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		if err := a.sched.Run(ctx); err != nil {
			a.logger.Error("scheduler stopped", "error", err)
		}
	}()

	serverErr := make(chan error, 1)
	go func() {
		a.logger.Info("control plane listening", "addr", a.cfg.Server.Addr, "owner", a.cfg.Owner)
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		a.logger.Info("shutting down")
	}

	// Give in-flight requests a moment, then stop. Fork VMs are deliberately
	// left running: nothing is reclaimed automatically, so work in progress
	// survives a control-plane restart.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		a.logger.Error("could not shut the server down cleanly", "error", err)
	}
	<-schedulerDone
	return nil
}

func (a *application) close() {
	if err := a.store.Close(); err != nil {
		a.logger.Error("could not close the database", "error", err)
	}
}

// buildModel returns the orchestrator's model client.
//
// The model is pluggable behind an OpenAI-shaped API, so switching providers
// is a base URL and a model name. With no key configured the control plane
// still starts and serves everything that does not need planning; the planning
// endpoints then fail with a message that says what is missing rather than a
// confusing provider error.
func buildModel(cfg config.Config, logger *slog.Logger) (llm.Client, error) {
	if cfg.LLM.APIKey == "" {
		return unconfiguredModel{}, nil
	}
	logger.Info("orchestrator model configured", "base_url", cfg.LLM.BaseURL, "model", cfg.LLM.Model)
	return llm.New(cfg.LLM, nil)
}

// unconfiguredModel stands in when no orchestrator credentials are set.
type unconfiguredModel struct{}

func (unconfiguredModel) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("no orchestrator model is configured; set %s", config.EnvOrchestrator)
}

func (unconfiguredModel) Model() string { return "unconfigured" }

// buildVerifier resolves the shared UI VM and builds the verifier around it.
//
// One UI VM serves every verification, so it is provisioned once here rather
// than per fork. Failing to get one is not fatal: the daemon still serves the
// control plane and the audit trail, and says plainly that nothing can be
// verified, which is better than refusing to start and leaving the operator
// without the UI that would explain why.
func buildVerifier(ctx context.Context, driver vm.Driver, cfg *config.Config, logger *slog.Logger) (*verify.Verifier, error) {
	if cfg.Verify.UIInstanceID == "" {
		if !cfg.Verify.VM.AutoProvision {
			return nil, nil
		}
		instance, err := verify.EnsureUIVM(ctx, driver, cfg.Verify.VM)
		if err != nil {
			logger.Warn("could not provision the shared UI VM; verification is unavailable and forks will stay queued",
				"error", err)
			return nil, nil
		}
		cfg.Verify.UIInstanceID = instance.ID
		logger.Info("shared UI VM ready",
			"instance", instance.ID, "address", instance.Address, "profiles", cfg.Verify.Profiles)
	}

	verifier, err := verify.New(driver, cfg.Verify)
	if err != nil {
		return nil, err
	}
	return verifier, nil
}

// buildDriver selects the VM driver.
func buildDriver(cfg config.Config, dataDir string) (vm.Driver, error) {
	switch cfg.Driver.Kind {
	case "firecracker":
		return firecracker.New(cfg.Driver.Firecracker)
	default:
		return local.New(local.Options{
			Root:         filepath.Join(dataDir, "vms"),
			Total:        cfg.Driver.Local.Total,
			MaxInstances: cfg.Driver.Local.MaxInstances,
		})
	}
}

// buildRouter selects how preview routes are published.
func buildRouter(cfg config.Config) proxy.Router {
	if cfg.Caddy.WildcardDomain == "" {
		return proxy.Discard{}
	}
	return caddy.New(cfg.Caddy, nil)
}

// nudgeFunc adapts a function to orchestrator.Notifier.
type nudgeFunc func()

func (f nudgeFunc) Nudge() { f() }

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
