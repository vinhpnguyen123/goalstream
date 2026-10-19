// Command ingest cuts goal clips from SoccerNet matches into object storage and Postgres.
//
//	ingest migrate   apply pending schema migrations (the same code the API runs at startup)
//	ingest run       ingest every match under $DATA_DIR/matches (idempotent: re-runs skip clips
//	                 already ingested), then regenerate $TAGS_TODO_PATH
//	ingest tags      apply the hand-filled $TAGS_TODO_PATH, then rebuild every embedding
//	ingest reembed   rebuild every embedding (after a vocab or layout change)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/ingest"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

func main() {
	// Ctrl-C cancels the context: the running ffmpeg is killed, the temp dir is removed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ingest:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ingest migrate|run|tags|reembed")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	switch args[0] {
	case "migrate":
		return db.Migrate(cfg.DatabaseURL)
	case "run":
		return runIngest(ctx, cfg)
	case "tags", "reembed":
		return rebuild(ctx, cfg, args[0])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runIngest(ctx context.Context, cfg config.Config) error {
	if cfg.Ingest.DataDir == "" {
		return fmt.Errorf("DATA_DIR is required for `ingest run`")
	}
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	r := &ingest.Runner{
		Pool:  pool,
		Store: storage.New(cfg.S3),
		Cfg:   cfg.Ingest,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	sum, runErr := r.Run(ctx, cfg.Ingest.DataDir)
	printSummary(sum) // also after a failure: it shows how far the run got
	if runErr != nil {
		return runErr
	}
	n, err := ingest.WriteTagsTodo(ctx, pool, cfg.Ingest.TagsTodoPath)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s: %d clip(s) waiting for hand-labelled tags\n", cfg.Ingest.TagsTodoPath, n)
	return nil
}

func rebuild(ctx context.Context, cfg config.Config, cmd string) error {
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cmd == "tags" {
		n, err := ingest.LoadTags(ctx, pool, cfg.Ingest.TagsTodoPath)
		if err != nil {
			return err
		}
		fmt.Printf("%d clip(s) re-tagged; every embedding rebuilt\n", n)
		return nil
	}
	var n int
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) (err error) {
		n, err = recommend.ReembedAll(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Printf("%d embedding(s) rebuilt\n", n)
	return nil
}

func printSummary(s ingest.Summary) {
	fmt.Printf("\nmatches scanned   %d\n", s.Matches)
	fmt.Printf("clips ingested    %d\n", s.Ingested)
	fmt.Printf("already present   %d\n", s.Present)
	fmt.Printf("ffmpeg cuts       %d\n", s.Cuts)
	reasons := make([]string, 0, len(s.Rejected))
	for r := range s.Rejected {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Printf("rejected          %-22s %d\n", r, s.Rejected[r])
	}
}
