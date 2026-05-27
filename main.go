package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"immich-bridge/bridge"
	"immich-bridge/config"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Flags
	var (
		configPath string
		dryRun     bool
		ruleFilter string
		noAuto     bool
		verbose    bool
	)

	fs := flag.NewFlagSet("immich-bridge", flag.ExitOnError)
	fs.StringVar(&configPath, "config", "./config.json", "Path to config file")
	fs.BoolVar(&dryRun, "dry-run", false, "Show what would be synced without acting")
	fs.StringVar(&ruleFilter, "rule", "", "Run only a specific sync rule by name")
	fs.BoolVar(&noAuto, "no-auto", false, "Disable auto-discovery, use only explicit sync_rules")
	fs.BoolVar(&verbose, "verbose", false, "Enable debug logging")

	command := os.Args[1]
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(1)
	}

	// Logging
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}

	setupLogging := func(level slog.Level) {
		handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
		slog.SetDefault(slog.New(handler))
	}
	setupLogging(logLevel)

	// Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Apply config-level log level unless --verbose was given
	if !verbose && cfg.LogLevel != "" {
		switch cfg.LogLevel {
		case "debug":
			setupLogging(slog.LevelDebug)
		case "warn":
			setupLogging(slog.LevelWarn)
		case "error":
			setupLogging(slog.LevelError)
		}
	}

	// Context with graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received shutdown signal", "signal", sig)
		cancel()
	}()

	b := bridge.New(cfg, dryRun, ruleFilter, noAuto)

	switch command {
	case "sync":
		if err := b.Validate(ctx); err != nil {
			slog.Error("validation failed", "error", err)
			os.Exit(1)
		}
		if err := b.RunOnce(ctx); err != nil {
			slog.Error("sync failed", "error", err)
			os.Exit(1)
		}

	case "daemon":
		if err := b.Validate(ctx); err != nil {
			slog.Error("validation failed", "error", err)
			os.Exit(1)
		}
		if err := b.RunDaemon(ctx); err != nil && ctx.Err() == nil {
			slog.Error("daemon failed", "error", err)
			os.Exit(1)
		}

	case "validate":
		if err := b.Validate(ctx); err != nil {
			slog.Error("validation failed", "error", err)
			os.Exit(1)
		}
		rules, err := b.DiscoverRules(ctx)
		if err != nil {
			slog.Error("discovery failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("Config OK — %d instance(s), %d sync rule(s) discovered\n",
			len(cfg.Instances), len(rules))
		for _, r := range rules {
			tag := ""
			if r.AutoDiscovered {
				tag = " [auto]"
			}
			fmt.Printf("  %s%s: %s/%s → %s/%s (delete=%v)\n",
				r.Name, tag,
				r.Source.Instance, r.Source.AlbumName,
				r.Destination.Instance, r.Destination.AlbumName,
				r.ShouldDeleteFromSource())
		}

	case "status":
		if err := b.Validate(ctx); err != nil {
			slog.Error("validation failed", "error", err)
			os.Exit(1)
		}
		if err := b.Status(ctx); err != nil {
			slog.Error("status failed", "error", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `immich-bridge — Multi-instance Immich photo sync

Usage:
  immich-bridge <command> [flags]

Commands:
  sync       Run a one-time sync for all rules
  daemon     Run continuously, polling at the configured interval
  validate   Validate config, test connectivity, list discovered rules
  status     Show discovered rules and pending asset counts

Flags:
  --config string    Path to config file (default "./config.json")
  --dry-run          Show what would be synced without acting
  --rule string      Run only a specific sync rule by name
  --no-auto          Disable auto-discovery, use only explicit sync_rules
  --verbose          Enable debug logging
`)
}
