package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "whatsfeatherd:", err)
		os.Exit(1)
	}
}

func run() error {
	syscall.Umask(0077)
	return runWithArgs(os.Args[1:], productionBootstrapDependencies())
}

func runWithArgs(args []string, deps bootstrapDependencies) error {
	flags := flag.NewFlagSet("whatsfeatherd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socket := flags.String("socket", "", "Private runtime Unix socket path")
	dataDir := flags.String("data-dir", "", "Profile root (encrypted ciphertext in --live; synthetic plaintext in --demo)")
	runtimeDir := flags.String("runtime-dir", "", "Private runtime directory (defaults to the socket's parent)")
	demo := flags.Bool("demo", false, "Use synthetic offline data in plaintext; never used as a live fallback")
	live := flags.Bool("live", false, "Require an encrypted profile; WhatsApp Connect remains explicit")
	liveTestScopePath := flags.String("live-test-scope", "", "Restrict live input to a private bounded scope file")
	migrationSource := flags.String("migrate-live-source", "", "Offline migration into a new encrypted destination; after interruption keep source and destination intact and retry to a different new --data-dir")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	migrationFlagSet, demoFlagSet, liveFlagSet, scopeFlagSet, socketFlagSet := false, false, false, false, false
	flags.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "migrate-live-source":
			migrationFlagSet = true
		case "demo":
			demoFlagSet = true
		case "live":
			liveFlagSet = true
		case "live-test-scope":
			scopeFlagSet = true
		case "socket":
			socketFlagSet = true
		}
	})
	if migrationFlagSet {
		if *migrationSource == "" || demoFlagSet || liveFlagSet || scopeFlagSet || socketFlagSet || flags.NArg() != 0 {
			return fmt.Errorf("--migrate-live-source is an exclusive offline command requiring --data-dir and --runtime-dir, with no storage-mode, scope, or socket flags")
		}
		return runOfflineMigration(ctx, *migrationSource, *dataDir, *runtimeDir, deps)
	}
	if *demo == *live {
		return fmt.Errorf("select exactly one explicit storage mode: --demo or --live")
	}
	var scope *whatsapp.LiveTestScope
	if scopeFlagSet {
		if !*live || *demo {
			return fmt.Errorf("live-test scope requires explicit --live mode")
		}
		if *liveTestScopePath == "" {
			return fmt.Errorf("live-test scope path is required")
		}
		var err error
		scope, err = whatsapp.LoadLiveTestScope(*liveTestScopePath)
		if err != nil {
			return err
		}
	}

	runtime, err := bootstrap(ctx, bootstrapOptions{
		dataDir: *dataDir, runtimeDir: *runtimeDir, socketPath: *socket,
		demo: *demo, live: *live, scope: scope,
	}, deps)
	if err != nil {
		return err
	}
	return runtime.Run(ctx)
}
