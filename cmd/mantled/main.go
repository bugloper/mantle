// Command mantled is the Mantle registry daemon.
//
// One binary, one database, no sidecars (§3.4). Background work — garbage
// collection, ledger writes, partition maintenance — runs in-process on a
// leader-elected node rather than in a second process (NG-2), so that
// "replace one file and restart" remains the whole upgrade procedure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/mantle-sh/mantle/internal/config"
	"github.com/mantle-sh/mantle/internal/observability"
	"github.com/mantle-sh/mantle/internal/server"
)

func main() {
	var (
		configPath  = flag.String("config", defaultConfigPath(), "path to the configuration file")
		showVersion = flag.Bool("version", false, "print the version and exit")
		checkOnly   = flag.Bool("check", false, "validate the configuration and exit without starting")
		bootstrap   = flag.Bool("bootstrap", false,
			"create the default organization and first administrator, then exit")
		bootstrapUser = flag.String("bootstrap-username", "admin",
			"username for the account created by --bootstrap")
		bootstrapOrg = flag.String("bootstrap-org", "",
			"default organization created by --bootstrap")
	)
	flag.Usage = usage
	flag.Parse()

	server.Version = resolveVersion()

	if *showVersion {
		fmt.Println("mantled", server.Version)
		return
	}

	// A missing config file is not an error when the path was not given
	// explicitly: an operator running mantled with everything in the
	// environment is a supported deployment, and demanding an empty file
	// would be pointless ceremony.
	path := *configPath
	if path != "" && !fileExists(path) {
		if isFlagSet("config") {
			fatal("configuration file %s does not exist", path)
		}
		path = ""
	}

	cfg, err := config.Load(path)
	if err != nil {
		fatal("%v", err)
	}

	logger := observability.NewLogger(os.Stderr,
		cfg.Observability.LogFormat, cfg.Observability.LogLevel)

	if *checkOnly {
		fmt.Println("configuration is valid")
		return
	}

	// SIGINT and SIGTERM begin a graceful drain: readiness starts failing so a
	// load balancer removes this node, then in-flight requests are given time
	// to finish. Without that ordering a rolling upgrade drops whatever was in
	// flight, which breaks the 100% pull-availability target in §16.3.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, logger)
	if err != nil {
		fatal("%v", err)
	}
	defer srv.Close()

	if *bootstrap {
		if err := runBootstrap(ctx, srv, *bootstrapUser, *bootstrapOrg); err != nil {
			fatal("%v", err)
		}
		return
	}

	if err := srv.Run(ctx); err != nil {
		logger.Error("mantled exited with an error", "error", err)
		os.Exit(1)
	}
	logger.Info("mantled stopped cleanly")
}

func usage() {
	fmt.Fprintf(os.Stderr, `mantled — the Mantle registry daemon

Usage:
  mantled [flags]

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Configuration is resolved from flags, then MANTLE_* environment variables, then
the configuration file, then built-in defaults. Every default is safe to run
with.

Examples:
  mantled --config /etc/mantle/mantle.yaml
  MANTLE_DATABASE_URL=postgres://mantle@localhost/mantle mantled
  mantled --check                 validate configuration without starting
`)
}

func defaultConfigPath() string {
	if path := os.Getenv("MANTLE_CONFIG"); path != "" {
		return path
	}
	return "/etc/mantle/mantle.yaml"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// resolveVersion prefers a version stamped at build time and falls back to the
// module's own build info, so a `go install` build still reports something
// meaningful rather than "dev".
func resolveVersion() string {
	if server.Version != "dev" && server.Version != "" {
		return server.Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mantled: "+format+"\n", args...)
	os.Exit(1)
}

// runBootstrap creates the default organization and the first instance
// administrator, printing the generated password exactly once.
//
// This is the one operation that legitimately runs before there is an API to
// call, because it creates the credential every API call needs. It is
// idempotent: re-running it after a partial install must never replace the
// credentials of a working instance.
func runBootstrap(ctx context.Context, srv *server.Server, username, org string) error {
	result, err := server.Bootstrap(ctx, srv.Pool, username, os.Getenv("MANTLE_ADMIN_PASSWORD"), org)
	if err != nil {
		return err
	}

	if result.AlreadyBootstrapped {
		fmt.Printf("This instance already has an administrator; nothing was changed.\n"+
			"  organization: %s\n"+
			"  To reset a password, use 'mantle user' from an account that still works.\n",
			result.Organization)
		return nil
	}

	fmt.Printf("Mantle is ready.\n\n")
	fmt.Printf("  organization  %s\n", result.Organization)
	fmt.Printf("  username      %s\n", result.AdminUsername)
	if result.AdminPassword != "" {
		fmt.Printf("  password      %s\n\n", result.AdminPassword)
		fmt.Printf("This password is shown once and is not recoverable. Store it now.\n\n")
	} else {
		fmt.Printf("  password      (as supplied in MANTLE_ADMIN_PASSWORD)\n\n")
	}
	fmt.Printf("Next:\n")
	fmt.Printf("  mantle login <registry-url> --username %s --token '<password>'\n", result.AdminUsername)
	fmt.Printf("  mantle setup --repo %s/<app>\n", result.Organization)
	return nil
}
