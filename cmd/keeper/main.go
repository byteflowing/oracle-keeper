// Command keeper is the oracle-keeper entry point.
//
// Usage:
//
//	oracle-keeper           # serve: hourly keep-alive cycles until signal
//	oracle-keeper once      # run a single cycle immediately, then exit
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/signalx"

	"github.com/servekit/oracle-keeper/internal/app"
	"github.com/servekit/oracle-keeper/internal/keeper"
	"github.com/servekit/oracle-keeper/pkg/config"
)

func main() {
	// Load .env when present so local binary runs pick up the same values
	// docker-compose injects. A missing .env (docker/prod, where env is
	// injected directly) is normal; only a malformed file warns.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "warning: failed to load .env:", err)
	}

	switch subcommand() {
	case "", "serve":
		if err := serve(); err != nil {
			slog.Error("serve failed", "error", err)
			os.Exit(1)
		}
	case "once":
		if err := once(); err != nil {
			slog.Error("once failed", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [serve|once]\n", os.Args[0])
		os.Exit(2)
	}
}

// serve runs the cron-scheduled daemon until SIGINT/SIGTERM.
func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logging.Setup(cfg.Log)
	slog.Info("starting", "service", "oracle-keeper")

	a, err := app.New(cfg)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	return signalx.RunWithForceQuit(a)
}

// once executes a single cycle immediately — the smoke test to run right
// after deploying, before trusting the hourly schedule.
func once() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logging.Setup(cfg.Log)

	kpr, err := keeper.New(cfg)
	if err != nil {
		return fmt.Errorf("init keeper: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Schedule.MaxRunDuration)
	defer cancel()
	return kpr.Run(ctx)
}

// --- internal helpers ---

// subcommand returns the first positional argument, or "" when none is given.
func subcommand() string {
	if len(os.Args) > 1 {
		return os.Args[1]
	}
	return ""
}
