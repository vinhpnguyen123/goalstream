// Package handlers holds the HTTP routes. Handlers stay thin: decode → internal/ → encode.
package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/middleware"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

type Server struct {
	Pool  *pgxpool.Pool
	Redis *redis.Client // rate limiter only in M1; it holds no truth
	Store *storage.Client
	Cfg   config.Config
	// DevUserID is the AUTH_MODE=dev user, upserted at startup. Empty in any other mode.
	DevUserID string
}

// Routes builds the router. Middleware order is docs/API.md § Middleware order; every route
// lives under /api/v1.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	byIP := middleware.ByIP(s.Cfg.API.ClientIPHeader)
	r.Use(middleware.RequestID, middleware.Recover, middleware.LimitBody,
		middleware.RateLimit(s.Redis, middleware.RuleDefault, byIP))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "not found") // no 405: the set is closed
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/healthz", s.healthz) // exempt from the gate, like POST /gate
		r.With(
			middleware.RateLimit(s.Redis, middleware.RuleGate, byIP),
			middleware.RateLimit(s.Redis, middleware.RuleGateGlobal, middleware.Global),
		).Post("/gate", s.gate)

		r.Group(func(r chi.Router) {
			r.Use(middleware.Gate(s.Cfg.API), middleware.Auth(s.Cfg.API.AuthMode, s.DevUserID))
			r.Get("/clips", s.listClips)
			r.With(middleware.RateLimit(s.Redis, middleware.RulePlay, middleware.ByUser)).
				Get("/clips/{id}/play", s.playClip)
			r.Post("/sessions", s.createSession)
			r.Post("/sessions/{id}/end", s.endSession)
			r.Get("/sessions/{id}/summary", s.sessionSummary)
			r.Get("/sessions/{id}/next", s.nextClip)
			r.Post("/feedback", s.postFeedback)
		})
	})
	return r
}

// healthz answers without touching dependencies: it says the process serves HTTP, which is
// what a deploy check needs. A Postgres outage must not make Render restart a healthy API.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
