// Command api is the HTTP API. It applies pending migrations before serving (decision D17).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/handlers"
)

const shutdownGrace = 15 * time.Second

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

	srv := &http.Server{
		Addr:              cfg.API.HTTPAddr,
		Handler:           (&handlers.Server{Pool: pool, Cfg: cfg}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
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
