// Command worker transcodes clips to HLS. By default it takes jobs from the Redis queue until
// SIGTERM, with the reaper sweeping alongside; -once runs one job by id and exits (M5, kept for
// debugging):
//
//	worker
//	worker -once <job_id>
//
// It applies pending migrations first (D17).
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

	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

func main() {
	// Ctrl-C / SIGTERM: no new job is taken; the one in hand gets SHUTDOWN_GRACE (-once: none),
	// then ffmpeg is killed and the job is recorded retryable with reason shutdown, its attempt
	// given back.
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
	if fs.NArg() > 0 {
		return errors.New("usage: worker [-once <job_id>]")
	}
	cfg, err := config.Load(os.Getenv)
	if err == nil {
		err = cfg.ValidateWorker()
	}
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
	if *once != "" {
		return w.Process(ctx, *once)
	}

	ropt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("config: REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(ropt) // not pinged: the loop backs off while Redis is down
	defer rdb.Close()
	log.Info("worker started", "transcode_timeout", cfg.Worker.TranscodeTimeout.String(),
		"shutdown_grace", cfg.Worker.ShutdownGrace.String())
	q := transcode.NewQueue(rdb)
	go (&transcode.Reaper{Pool: pool, Q: q, Cfg: cfg.Worker, Log: log}).Run(ctx)
	(&transcode.Loop{Q: q, W: w, Log: log}).Run(ctx)
	log.Info("worker stopped")
	return nil
}
