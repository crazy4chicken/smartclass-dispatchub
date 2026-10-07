// Command dispatchub runs the SmartClass recording dispatch service.
//
// Subcommands: run, status, doctor, register-permissions, migrate. Run
// `dispatchub` without arguments to print the usage; unknown subcommands exit
// with status 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	// Embed the IANA time zone database so DISPATCH_TIMEZONE resolves on hosts
	// without system tzdata, such as a Windows development box or a scratch
	// container. It costs roughly 400 KiB of binary size.
	_ "time/tzdata"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/config"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/httpapi"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/iamauth"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/scheduler"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// version is set at build time with -ldflags "-X main.version=<version>".
var version = "dev"

const (
	serviceName     = "dispatchub"
	shutdownTimeout = 10 * time.Second
	doctorTimeout   = 30 * time.Second
	registerTimeout = 60 * time.Second
	drainTimeout    = 2 * time.Second
	// envAdminToken carries the teamusers admin bearer token used by
	// register-permissions when -token is not given.
	envAdminToken = "DISPATCH_TEAMUSERS_ADMIN_TOKEN"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches one subcommand and returns the process exit code.
func run(args []string) int {
	if len(args) > 0 && (args[0] == "-version" || args[0] == "--version") {
		fmt.Println(version)
		return 0
	}
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	command := args[0]
	args = args[1:]
	switch command {
	case "-h", "-help", "--help":
		usage(os.Stdout)
		return 0
	case "run", "status", "doctor", "register-permissions", "migrate":
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", command)
		usage(os.Stderr)
		return 2
	}

	flags := flag.NewFlagSet(serviceName+" "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	adminToken := ""
	if command == "register-permissions" {
		flags.StringVar(&adminToken, "token", os.Getenv(envAdminToken), "teamusers admin bearer token")
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	if err := validateCommandConfig(command, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	logger := newLogger(cfg)

	switch command {
	case "run":
		return runServer(cfg, logger)
	case "status":
		return runStatus(cfg, logger)
	case "doctor":
		return runDoctor(cfg, logger)
	case "register-permissions":
		return runRegisterPermissions(cfg, logger, adminToken)
	case "migrate":
		return runMigrate(cfg, logger)
	}
	// Unreachable: every command was validated before the configuration was
	// loaded, so the switch above always returns. This keeps the compiler
	// happy if a command is ever added to only one of the two switches.
	return 2
}

// validateCommandConfig enforces the runtime dependencies of one subcommand.
// FromEnv has already rejected malformed values, so a command only refuses to
// start over what it truly cannot run without: the server needs every endpoint
// and credential, migrate needs the database and register-permissions needs the
// teamusers base URL. status and doctor need none of them, because they report
// a missing dependency as SKIP instead of failing.
func validateCommandConfig(command string, cfg config.Config) error {
	switch command {
	case "run":
		return cfg.ValidateServer()
	case "migrate":
		return cfg.ValidateDatabase()
	case "register-permissions":
		return cfg.ValidateTeamusers()
	default:
		return nil
	}
}

// usage prints the subcommand summary.
func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: %s <command>

commands:
  run                   serve the HTTP API and run the recording scheduler
  status                print the redacted configuration and dependency reachability
  doctor                run the status checks with a %s timeout; exit non-zero on failure
  migrate               apply the embedded goose migrations and exit
  register-permissions  upsert the dispatch:* permission catalog in teamusers (-token)

`, serviceName, doctorTimeout)
}

// newLogger builds the JSON logger at the configured level.
func newLogger(cfg config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Level()}))
}

// iamauthOptions maps the service configuration onto the IAM wiring.
func iamauthOptions(cfg config.Config, log *slog.Logger) iamauth.Options {
	return iamauth.Options{
		BaseURL:      cfg.TeamusersURL,
		Issuer:       cfg.TeamusersIssuer,
		Audience:     cfg.TeamusersAudience,
		ClientID:     cfg.TeamusersClientID,
		ClientSecret: cfg.TeamusersClientSecret,
		Timeout:      cfg.TeamusersTimeout,
		Logger:       log,
		Dev:          cfg.Dev,
	}
}

// runServer serves traffic and runs the scheduler until a signal arrives.
func runServer(cfg config.Config, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		log.Error("open database", "error", err)
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx, log); err != nil {
		log.Error("apply migrations", "error", err)
		return 1
	}

	auth, err := iamauth.New(iamauthOptions(cfg, log))
	if err != nil {
		log.Error("init iam authorizer", "error", err)
		return 1
	}
	defer auth.Close()

	cameras := webcam.New(cfg.WebcamURL, auth, cfg.WebcamTimeout, log)
	tasks := scheduler.New(scheduler.Deps{Store: db, Webcam: cameras, Config: cfg, Logger: log})

	schedulerErr := make(chan error, 1)
	go func() {
		schedulerErr <- tasks.Run(ctx)
	}()

	handler := httpapi.NewRouter(httpapi.Deps{
		Store:    db,
		Commands: tasks,
		Webcam:   cameras,
		Auth:     auth,
		Config:   cfg,
		Logger:   log,
	})
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen", "addr", cfg.ListenAddr, "error", err)
		return 1
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	log.Info("dispatchub serving", "addr", listener.Addr().String(), "version", version, "dev", cfg.Dev)

	failure := false
	schedulerDrained := false
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			failure = true
		}
	case err := <-schedulerErr:
		schedulerDrained = true
		if ctx.Err() == nil {
			// Run blocks until its context is cancelled, so exiting on its own
			// means the scheduler died and recordings would silently stop.
			log.Error("scheduler stopped unexpectedly", "error", err)
			failure = true
		} else if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("scheduler stopped", "error", err)
			failure = true
		}
	case <-ctx.Done():
	}
	stop()
	if !schedulerDrained {
		select {
		case <-schedulerErr:
		case <-time.After(drainTimeout):
		}
	}

	log.Info("shutting down", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out, closing", "error", err)
		_ = server.Close()
	}
	log.Info("shutdown complete")
	if failure {
		return 1
	}
	return 0
}

// checker renders PASS, FAIL and SKIP lines for status and doctor.
type checker struct {
	ctx    context.Context
	failed bool
}

func (c *checker) pass(name, detail string) {
	fmt.Printf("PASS %-10s %s\n", name, detail)
}

func (c *checker) fail(name string, err error) {
	c.failed = true
	fmt.Printf("FAIL %-10s %v\n", name, err)
}

func (c *checker) skip(name, detail string) {
	fmt.Printf("SKIP %-10s %s\n", name, detail)
}

// printRedacted prints the effective configuration with every secret masked.
func printRedacted(cfg config.Config) {
	for _, line := range cfg.Redacted() {
		fmt.Println(line)
	}
}

// runStatus prints the redacted configuration and the dependency
// reachability. It always exits 0 once the configuration loaded.
func runStatus(cfg config.Config, log *slog.Logger) int {
	printRedacted(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	c := &checker{ctx: ctx}
	c.postgres(cfg)
	c.webcam(cfg, log)
	return 0
}

// runDoctor runs the same checks as status and exits non-zero when one fails.
func runDoctor(cfg config.Config, log *slog.Logger) int {
	printRedacted(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	c := &checker{ctx: ctx}
	c.postgres(cfg)
	c.webcam(cfg, log)
	if c.failed {
		return 1
	}
	return 0
}

// postgres opens the pool, pings it and reports the server version.
func (c *checker) postgres(cfg config.Config) {
	if strings.TrimSpace(cfg.DSN) == "" {
		c.skip("postgres", config.EnvDSN+" is not configured")
		return
	}
	db, err := store.Open(c.ctx, cfg.DSN)
	if err != nil {
		c.fail("postgres", err)
		return
	}
	defer db.Close()
	version := "unknown"
	if db.Pool != nil {
		if err := db.Pool.QueryRow(c.ctx, "SELECT current_setting('server_version')").Scan(&version); err != nil {
			version = "unknown (" + err.Error() + ")"
		}
	}
	c.pass("postgres", "connected, server_version "+version)
}

// webcam probes webcam-server's /readyz without a service token; the probe is
// public.
func (c *checker) webcam(cfg config.Config, log *slog.Logger) {
	if strings.TrimSpace(cfg.WebcamURL) == "" {
		c.skip("webcam", config.EnvWebcamURL+" is not configured")
		return
	}
	client := webcam.New(cfg.WebcamURL, webcam.TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("readiness probe requires no service token")
	}), cfg.WebcamTimeout, log)
	if err := client.Ready(c.ctx); err != nil {
		c.fail("webcam", err)
		return
	}
	c.pass("webcam", "reachable: "+cfg.WebcamURL)
}

// runRegisterPermissions upserts the dispatch:* catalog in teamusers. The
// teamusers base URL is guaranteed by validateCommandConfig.
func runRegisterPermissions(cfg config.Config, log *slog.Logger, token string) int {
	if strings.TrimSpace(token) == "" {
		log.Error("invalid configuration", "error", "an admin bearer token is required (-token or "+envAdminToken+")")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	defer cancel()
	auth, err := iamauth.New(iamauthOptions(cfg, log))
	if err != nil {
		log.Error("init iam authorizer", "error", err)
		return 1
	}
	defer auth.Close()
	if err := auth.RegisterPermissions(ctx, token, iamauth.PermissionKeys); err != nil {
		log.Error("register permissions", "error", err)
		return 1
	}
	for _, key := range iamauth.PermissionKeys {
		fmt.Println(key)
	}
	return 0
}

// runMigrate applies the embedded migrations and exits.
func runMigrate(cfg config.Config, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		log.Error("open database", "error", err)
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx, log); err != nil {
		log.Error("apply migrations", "error", err)
		return 1
	}
	log.Info("migrations applied", "version", version)
	return 0
}
