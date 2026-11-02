// Command api is the HTTP API. It applies pending migrations before serving (decision D17).
package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/handlers"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

const (
	shutdownGrace = 15 * time.Second
	pruneEvery    = time.Hour
	pruneTimeout  = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("api exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err == nil {
		err = cfg.ValidateAPI()
	}
	if err != nil {
		return fmt.Errorf("config: %w", err) // the startup guards (test #13) exit here
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("config: LOG_LEVEL: %w", err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	var devUserID string
	if cfg.API.AuthMode == "dev" {
		if devUserID, err = auth.UpsertDevUser(ctx, pool, cfg.API.DevUserEmail); err != nil {
			return err
		}
	}
	var signer *auth.Signer
	if cfg.API.AuthMode == "jwt" {
		signer = auth.NewSigner(cfg.API.JWTSecret, cfg.API.AccessTokenTTL)
	}
	ropt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("config: REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(ropt) // not pinged: the limiter decides per rule what an outage means
	defer rdb.Close()

	var seed [32]byte
	if _, err := crand.Read(seed[:]); err != nil {
		return fmt.Errorf("seeding the recommender: %w", err)
	}
	reco := recommend.NewRecommender(pool, cfg.Reco, rand.New(rand.NewChaCha8(seed)))

	srv := &http.Server{
		Addr: cfg.API.HTTPAddr,
		Handler: (&handlers.Server{Pool: pool, Redis: rdb, Queue: transcode.NewQueue(rdb), Store: storage.New(cfg.S3), Cfg: cfg,
			DevUserID: devUserID, Signer: signer, Reco: reco}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	if cfg.API.AuthMode == "jwt" {
		go pruneLoop(ctx, pool)
	}
	slog.Info("api listening", "addr", cfg.API.HTTPAddr, "app_env", cfg.AppEnv, "auth_mode", cfg.API.AuthMode,
		"gate_enabled", cfg.API.GateEnabled)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// pruneLoop deletes refresh tokens expired for more than a day, hourly, until shutdown. A
// failed run is logged and retried on the next tick: stale rows are inert, not urgent.
func pruneLoop(ctx context.Context, pool *pgxpool.Pool) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pruneTimeout)
		n, err := auth.PruneRefresh(pctx, pool)
		cancel()
		if err != nil {
			slog.Error("pruning refresh tokens", "err", err)
			continue
		}
		slog.Info("pruned refresh tokens", "deleted", n)
	}
}
