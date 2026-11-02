// Command worker transcodes clips to HLS. In M5 it runs one job and exits:
//
//	worker -once <job_id>
//
// The queue loop (BLMOVE), the reaper and graceful shutdown arrive in M6 around the same
// claim → attempt → fenced commit functions. It applies pending migrations first (D17).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

func main() {
	// Ctrl-C / SIGTERM cancels the attempt: ffmpeg is killed and the job is recorded as
	// retryable with reason shutdown, its attempt given back.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("worker exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	once := fs.String("once", "", "run this transcode job id once and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *once == "" {
		return errors.New("usage: worker -once <job_id> (the queue loop arrives in M6)")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("config: LOG_LEVEL: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	w := &transcode.Worker{Pool: pool, Store: storage.New(cfg.S3), Cfg: cfg.Worker, Log: log}
	return w.Process(ctx, *once)
}
