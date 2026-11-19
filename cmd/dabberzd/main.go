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
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/dabbers/devex/internal/agent"
	"github.com/dabbers/devex/internal/agentauth"
	"github.com/dabbers/devex/internal/api"
	"github.com/dabbers/devex/internal/config"
	"github.com/dabbers/devex/internal/database"
	"github.com/dabbers/devex/internal/gitauth"
	"github.com/dabbers/devex/internal/identity"
	"github.com/dabbers/devex/internal/install"
	"github.com/dabbers/devex/internal/llm"
	"github.com/dabbers/devex/internal/memory"
	"github.com/dabbers/devex/internal/merge"
	"github.com/dabbers/devex/internal/orchestrator"
	"github.com/dabbers/devex/internal/pipeline"
	"github.com/dabbers/devex/internal/preview"
	"github.com/dabbers/devex/internal/previewpass"
	"github.com/dabbers/devex/internal/proxy"
	"github.com/dabbers/devex/internal/proxy/caddy"
	"github.com/dabbers/devex/internal/proxy/hostmux"
	"github.com/dabbers/devex/internal/scheduler"
	"github.com/dabbers/devex/internal/secrets"
	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/verify"
	"github.com/dabbers/devex/internal/vm"
	"github.com/dabbers/devex/internal/vm/firecracker"
	"github.com/dabbers/devex/internal/vm/local"
	"github.com/dabbers/devex/internal/web"
	"github.com/dabbers/devex/internal/workspace"
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
	// workspaces is resumed once the server is listening, so whatever was
	// running before a restart is running again without anybody asking.
	workspaces *workspace.Manager
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
	// An agent run lives in a goroutine, so a restart ends it without ending
	// the row it was writing. Left running, a workspace shows an agent working
	// forever and refuses every later message, with nothing to point at.
	if abandoned, err := db.AbandonRunningMessages(ctx,
		"the control plane restarted while this was running"); err != nil {
		return nil, err
	} else if abandoned > 0 {
		logger.Warn("abandoned agent turns left running by a previous run",
			"count", abandoned)
	}

	authenticator, err := cfg.Authenticator()
	if err != nil {
		return nil, err
	}
	logger.Info("authentication ready", "provider", authenticator.Name())

	// One translation from a credential to a dabberz user, shared by the two
	// front doors: the control plane's own API, and the edge that fronts
	// installations and has to tell an app who is asking.
	visitors, err := identity.New(authenticator, db)
	if err != nil {
		return nil, err
	}
	// Tokens this deployment issued itself, for everything that is not a
	// browser: the command line tool, a script, a cron job. A provider's
	// session token lasts about a minute, which is right for a page that
	// refreshes it and useless for anything else.
	visitors = visitors.WithTokens(db)

	// The owner row is created ahead of any request so a fresh installation
	// has one, and so an existing single-user database is the row the first
	// provider sign-in adopts rather than being left orphaned beside a new
	// one. Users are otherwise created on first sign-in.
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
	if driver.Name() == local.Name {
		// The agent acts without asking, which is safe because it is confined
		// to a machine of its own. This driver does not confine it to
		// anything: it runs on the host, as this process, with this process's
		// access. Correct for development and wrong for anything else, so it
		// is said rather than left to be inferred from a config file.
		logger.Warn("the local driver provides no isolation; the agent runs on this host " +
			"with this process's access, and acts without asking -- development only")
	}

	router, hostRouter := buildRouter(cfg, visitors, logger)
	// One issuer for the edge that checks passes and the installer that mints
	// them, with a key only this process holds. Only for an edge in this
	// process: an external router would not understand a pass, and minting
	// one for it would hand the verifier a credential nothing accepts.
	var passes *previewpass.Issuer
	if hostRouter != nil {
		passes, err = previewpass.NewRandom()
		if err != nil {
			return nil, err
		}
		hostRouter.WithPasses(passes)
		if cfg.Preview.PanelURL != "" {
			hostRouter.WithSignIn(cfg.Preview.PanelURL, cfg.Preview.Scheme, passes)
		} else {
			logger.Warn("preview.panel_url is not set; members-only pages on apps are refused, " +
				"since there is nowhere to sign in")
		}
	}
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

	// Each account's agent runs are driven by that account's own
	// subscription credential, stored sealed in the vault. The daemon's
	// Claude token is consulted only for the owner, and only when the
	// configuration asks for that.
	agentOpts := agentauth.Options{Vault: vault}
	if cfg.Claude.OwnerFallback {
		agentOpts.FallbackToken, agentOpts.FallbackUserID = cfg.Claude.Token, owner.ID
	}
	agentCreds, err := agentauth.New(agentOpts)
	if err != nil {
		return nil, err
	}
	agentRunner := agent.New(driver, agentCreds, cfg.Agent)

	verifier, err := buildVerifier(ctx, driver, &cfg, logger)
	if err != nil {
		return nil, err
	}

	// One path from "a repo on a branch" to "a machine with services running
	// and previews answering", shared by forks and by workspaces opened by
	// hand. Written twice the two would drift, and the half that gets less use
	// would be the half that breaks.
	// Each account clones with its own GitHub connection rather than with one
	// shared deploy token. The token is never stored: the identity provider
	// holds it, and it is fetched when a clone needs one.
	gitTokens := gitauth.New(gitauth.Options{
		SecretKey: cfg.Auth.Clerk.SecretKey,
		Users:     db,
	})
	if gitTokens.Configured() {
		logger.Info("per-account git credentials ready", "provider", gitauth.Provider)
	}

	// The data block: the Postgres cluster installations are provisioned on.
	// Optional, and a deployment without one still runs workspaces -- it just
	// cannot install an app that declares a database, and says so when asked.
	var databases *database.Block
	if cfg.Data.AdminDSN != "" {
		databases, err = database.Open(database.Config{
			AdminDSN:    cfg.Data.AdminDSN,
			PublicHost:  cfg.Data.PublicHost,
			SnapshotDir: snapshotDir(cfg),
		})
		if err != nil {
			return nil, err
		}
		if err := databases.Ping(ctx); err != nil {
			logger.Warn("the data block is configured but unreachable; installs needing a database will fail",
				"error", err)
		} else {
			logger.Info("data block ready", "snapshots", snapshotDir(cfg))
		}
	} else {
		logger.Warn("no data block (set " + config.EnvDataAdminDSN + "); apps needing a database cannot be installed")
	}

	var provisioner install.Databases
	if databases != nil {
		provisioner = databases
	}

	workspaces, err := workspace.New(workspace.Deps{
		Store: db, Driver: driver, Preview: allocator, Vault: vault,
		GitTokens: gitTokens,
		// Which database this particular workspace belongs to: an
		// installation's own, or a change's copy of it.
		Overrides: install.NewData(db, provisioner),
	}, workspace.Options{
		Resources: cfg.Driver.ForkResources,
		Image:     cfg.Driver.Image,
		Logger:    logger,
	})
	if err != nil {
		return nil, err
	}

	var launcher scheduler.Launcher
	if verifier != nil {
		pipe, err := pipeline.New(pipeline.Deps{
			Store: db, Driver: driver, Agent: agentRunner, Verifier: verifier,
			Reviewer: merge.New(driver, agentRunner, cfg.Merge),
			Preview:  allocator, Orch: orch,
			Workspaces: workspaces,
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

	// The catalog, and the installer that turns an entry in it into something
	// a customer is using.
	var installer *install.Installer
	if cfg.Catalog.Dir != "" {
		catalog, err := install.OpenCatalog(cfg.Catalog.Dir)
		if err != nil {
			return nil, err
		}
		// Workspaces clone from and push to these repositories, so whoever
		// the workspaces run as has to own them.
		var repoOwner *install.Owner
		if cfg.Driver.Kind != "firecracker" {
			runAs, err := lookupRunAs(cfg.Driver.Local.RunAs)
			if err != nil {
				return nil, err
			}
			if runAs != nil {
				repoOwner = &install.Owner{UID: int(runAs.UID), GID: int(runAs.GID)}
			}
		}
		deps := install.Deps{
			Store: db, Catalog: catalog, Workspaces: workspaces,
			Vault: vault, Databases: provisioner, Verifier: verifier,
			Logger: logger,
		}
		// Set only when there is one: a nil issuer stored in the interface
		// would read as present and fail on the first check.
		if passes != nil {
			deps.Passes = passes
		}
		installer, err = install.New(deps, install.Options{
			RepoDir: filepath.Join(cfg.DataDir, "repos"),
			// What the blocks that exist offer. Identity is the platform's
			// own, so an app behind the edge is told who is asking.
			Provided:  providedBlocks(cfg, authenticator.Name()),
			RepoOwner: repoOwner,
		})
		if err != nil {
			return nil, err
		}
		if verifier == nil {
			// Said out loud, because the failure is silent otherwise: a
			// change still reports passing checks, having been judged only by
			// its own suite, which notices a broken query and not a blank
			// page.
			logger.Warn("no shared UI VM configured; a change's preview will not be opened in a browser, " +
				"so its checks cannot notice a page that renders nothing")
		}
		apps, problems := catalog.List()
		for _, problem := range problems {
			logger.Warn("an app in the catalog does not satisfy the contract", "error", problem)
		}
		logger.Info("catalog ready", "dir", cfg.Catalog.Dir, "apps", len(apps))
	}

	ui, err := web.Handler()
	if err != nil {
		return nil, err
	}

	apiServer, err := api.New(api.Deps{
		Store: db, Orch: orch, Sched: sched, Preview: allocator,
		Vault: vault, Memory: mem, Verifier: verifier, Driver: driver,
		UI: ui, Authn: authenticator, Visitors: visitors, Workspaces: workspaces,
		Agent: agentRunner, GitTokens: gitTokens, AgentCreds: agentCreds,
		Installer: installer, Logger: logger, EdgeSignIn: edgeSignIn(hostRouter),
	})
	if err != nil {
		return nil, err
	}

	// Previews and the control plane share one listener, so one hostname can
	// be the panel and the rest previews. The mux routes published preview
	// hostnames to their fork and hands everything else to the control plane,
	// which is why the fallback can only be installed now: the handler it
	// falls back to did not exist until this point.
	handler := apiServer.Handler()
	if hostRouter != nil {
		hostRouter.SetFallback(handler)
		handler = hostRouter
		logger.Info("serving previews in-process", "domain", cfg.Preview.Domain)
	}

	wired = true
	return &application{
		cfg:        cfg,
		logger:     logger,
		store:      db,
		sched:      sched,
		workspaces: workspaces,
		server: &http.Server{
			Addr:        cfg.Server.Addr,
			Handler:     handler,
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

	// After the listener, not before: rebuilding every installation takes a
	// while, and the panel being unreachable for that long would be a second
	// outage on top of the one this repairs. A preview asked for in the
	// meantime gets the edge's "not answering yet" page, which is true.
	go func() {
		if err := a.workspaces.Resume(ctx); err != nil {
			a.logger.Error("not everything came back after the restart", "error", err)
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
		runAs, err := lookupRunAs(cfg.Driver.Local.RunAs)
		if err != nil {
			return nil, err
		}
		return local.New(local.Options{
			Root:         filepath.Join(dataDir, "vms"),
			Total:        cfg.Driver.Local.Total,
			MaxInstances: cfg.Driver.Local.MaxInstances,
			RunAs:        runAs,
		})
	}
}

// lookupRunAs resolves the account instances run as. A name that does not
// resolve stops the daemon: falling back to running as root would be the one
// outcome the setting exists to prevent.
func lookupRunAs(name string) (*local.User, error) {
	if name == "" {
		return nil, nil
	}
	account, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("driver.local.run_as: %w", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("driver.local.run_as: uid %q: %w", account.Uid, err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("driver.local.run_as: gid %q: %w", account.Gid, err)
	}
	if uid == 0 {
		return nil, fmt.Errorf("driver.local.run_as: %s is root", name)
	}
	return &local.User{UID: uint32(uid), GID: uint32(gid)}, nil
}

// buildRouter selects how preview routes are published, returning the router
// and, when previews are served from this process, the handler that serves
// them.
func buildRouter(cfg config.Config, who hostmux.Identities, logger *slog.Logger) (proxy.Router, *hostmux.Mux) {
	switch cfg.PreviewRouter() {
	case preview.RouterCaddy:
		return caddy.New(cfg.Caddy, nil), nil
	case preview.RouterNone:
		return proxy.Discard{}, nil
	default:
		mux := hostmux.New(cfg.Preview.Domain, who, logger)
		return mux, mux
	}
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

// snapshotDir is where database snapshots are written.
//
// Under the data directory by default, which is correct for getting started
// and wrong for keeping: a snapshot on the same disk as the database it
// protects survives a mistake and not a failure. Point it at mounted storage
// before anything real is on here.
func snapshotDir(cfg config.Config) string {
	if cfg.Data.SnapshotDir != "" {
		return cfg.Data.SnapshotDir
	}
	return filepath.Join(cfg.DataDir, "snapshots")
}

// providedBlocks is what this deployment offers an installed app.
//
// Assembled from configuration rather than assumed, so an app that declares
// something this deployment does not have is refused at install with the
// reason, rather than started and left to fail later in a way nobody connects
// back to a missing block.
func providedBlocks(cfg config.Config, issuer string) install.Provided {
	provided := install.Provided{
		// Identity is the platform's own: an app behind the edge is told who
		// is asking rather than implementing sign-in itself.
		Identity: install.Identity{Issuer: issuer, ClientID: "dabberz"},
		MailURL:  cfg.Blocks.MailURL,
		Storage: install.Storage{
			Endpoint:  cfg.Blocks.Storage.Endpoint,
			Bucket:    cfg.Blocks.Storage.Bucket,
			AccessKey: cfg.Blocks.Storage.AccessKey,
			SecretKey: cfg.Blocks.Storage.SecretKey,
		},
		RealtimeURL: cfg.Blocks.RealtimeURL,
		SearchURL:   cfg.Blocks.SearchURL,
	}
	return provided
}

// edgeSignIn is the in-process edge as the API sees it, or nothing. Spelled
// out because a nil *hostmux.Mux in an interface is not a nil interface, and
// the API decides whether sign-in exists by comparing with nil.
func edgeSignIn(mux *hostmux.Mux) api.EdgeSignIn {
	if mux == nil {
		return nil
	}
	return mux
}
