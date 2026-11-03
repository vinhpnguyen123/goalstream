// Command ingest cuts goal clips from SoccerNet matches into object storage and Postgres.
//
//	ingest migrate   apply pending schema migrations (the same code the API runs at startup)
//	ingest run       ingest every match under $DATA_DIR/matches (idempotent: re-runs skip clips
//	                 already ingested), then regenerate $TAGS_TODO_PATH
//	ingest tags      apply the hand-filled $TAGS_TODO_PATH, then rebuild every embedding
//	ingest reembed   rebuild every embedding (after a vocab or layout change)
//	ingest admin -email <email> [-revoke]
//	                 grant (or revoke) users.is_admin — the only way it changes
//	ingest transcode-batch [-limit N] [-wait]
//	                 queue a transcode for the next N mp4 clips (0 = all) that were never migrated
//	                 and have no active job; -wait reports until every job is done or failed.
//	                 The worker (make worker) does the transcoding.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/ingest"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
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
		return fmt.Errorf("usage: ingest migrate|run|tags|reembed|admin|transcode-batch")
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
	case "admin":
		return setAdmin(ctx, cfg, args[1:])
	case "transcode-batch":
		return transcodeBatch(ctx, cfg, args[1:])
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

// setAdmin is `make admin EMAIL=… [REVOKE=1]`. The change shows on the user's next request:
// the API reads is_admin from Postgres every time.
func setAdmin(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	raw := fs.String("email", "", "the user's email")
	revoke := fs.Bool("revoke", false, "revoke instead of grant")
	if err := fs.Parse(args); err != nil {
		return err
	}
	email, ok := auth.NormalizeEmail(*raw)
	if !ok {
		return fmt.Errorf("admin: -email must be a valid email, got %q", *raw)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	changed, err := auth.SetAdmin(ctx, pool, email, !*revoke)
	if errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("admin: no user with email %s (register first)", email)
	} else if err != nil {
		return err
	}
	state := map[bool]string{false: "is now admin", true: "is no longer admin"}[*revoke]
	if !changed {
		state = map[bool]string{false: "was already admin", true: "was already not admin"}[*revoke]
	}
	fmt.Println(email, state)
	return nil
}

// transcodeBatch is `make batch [LIMIT=N]`, the catalogue migration (M7): queue the next N
// never-migrated mp4 clips, then with -wait poll until every job ends and print the batch's
// success rates. Re-running it picks up only what is left, so "a few, verify, then the rest"
// is two runs. Ctrl-C stops the waiting, not the jobs — the worker keeps going.
func transcodeBatch(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("transcode-batch", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "how many clips to queue (0 = all pending)")
	waitFor := fs.Bool("wait", false, "report until every queued job is done or failed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	ropt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("config: REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(ropt)
	defer rdb.Close()

	clips, err := transcode.Pending(ctx, pool, *limit)
	if err != nil {
		return err
	}
	start := time.Now()
	jobs, err := transcode.Enqueue(ctx, pool, transcode.NewQueue(rdb), clips, slog.Default())
	fmt.Printf("queued %d jobs (%d clips pending)\n", len(jobs), len(clips))
	if err != nil || !*waitFor || len(jobs) == 0 {
		return err
	}

	fmt.Println("waiting — the worker (make worker) must be running; Ctrl-C stops waiting, not the jobs")
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	last := ""
	for {
		r, err := transcode.Progress(ctx, pool, jobs)
		if err != nil {
			return err
		}
		if line := fmt.Sprintf("active %d · done %d · failed %d", r.Active, r.Done, r.Failed); line != last {
			fmt.Printf("%s  %s\n", time.Since(start).Round(time.Second), line)
			last = line
		}
		if r.Active == 0 {
			printReport(r, time.Since(start))
			// Non-zero exit on any failure, so `make batch && make verify-hls` stops: a failed
			// clip is still mp4, and the migration is not done until none is.
			if r.Failed > 0 {
				return fmt.Errorf("%d of %d jobs failed", r.Failed, r.Done+r.Failed)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// printReport prints both success rates (plan §15 M7 Q4): job-level hides a failure that a
// retry fixed, so first-try is reported next to it.
func printReport(r transcode.Report, took time.Duration) {
	n := r.Done + r.Failed
	pct := func(k int) float64 { return 100 * float64(k) / float64(n) }
	fmt.Printf("\nbatch of %d jobs in %s\n", n, took.Round(time.Second))
	fmt.Printf("  success      %d/%d = %.1f%%\n", r.Done, n, pct(r.Done))
	fmt.Printf("  first try    %d/%d = %.1f%%\n", r.FirstTry, n, pct(r.FirstTry))
	var hist []string
	for a := 0; len(hist) < len(r.Attempts); a++ { // 0: cancelled before any claim
		if k, ok := r.Attempts[a]; ok {
			hist = append(hist, fmt.Sprintf("%d×%d", k, a))
		}
	}
	fmt.Printf("  attempts     %s  (jobs × attempts)\n", strings.Join(hist, " · "))
	for reason, k := range r.Reasons {
		fmt.Printf("  failed       %d × %s\n", k, reason)
	}
}
