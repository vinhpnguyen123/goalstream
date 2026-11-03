// Package handlers holds the HTTP routes. Handlers stay thin: decode → internal/ → encode.
package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/middleware"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

type Server struct {
	Pool  *pgxpool.Pool
	Redis *redis.Client // rate limiter, and the transcode queue through Queue; it holds no truth
	Queue transcode.Queue
	Store *storage.Client
	Cfg   config.Config
	// DevUserID is the AUTH_MODE=dev user, upserted at startup. Empty in any other mode.
	DevUserID string
	// Signer issues and verifies access tokens. Set only when AUTH_MODE=jwt, the only mode with
	// a JWT_SECRET (config enforces ≥ 32 bytes there).
	Signer *auth.Signer
	// Reco selects GET /next's clip (plan §11).
	Reco *recommend.Recommender
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

		// Gated, but before authentication: these are how a caller gets authenticated. Mounted
		// only in jwt mode — dev and closed have no JWT_SECRET, and a token signed with an empty
		// key must never exist.
		if s.Cfg.API.AuthMode == "jwt" {
			r.Group(func(r chi.Router) {
				r.Use(middleware.Gate(s.Cfg.API))
				r.With(middleware.RateLimit(s.Redis, middleware.RuleRegister, byIP)).Post("/auth/register", s.register)
				r.With(middleware.RateLimit(s.Redis, middleware.RuleLogin, byIP)).Post("/auth/login", s.login)
				r.With(middleware.RateLimit(s.Redis, middleware.RuleRefresh, byIP)).Post("/auth/refresh", s.refresh)
				r.Post("/auth/logout", s.logout)
			})
		}

		r.Group(func(r chi.Router) {
			r.Use(middleware.Gate(s.Cfg.API), middleware.Auth(s.Cfg.API.AuthMode, s.DevUserID, s.Signer))
			r.Get("/clips", s.listClips)
			r.With(middleware.RateLimit(s.Redis, middleware.RulePlay, middleware.ByUser)).
				Get("/clips/{id}/play", s.playClip)
			r.Post("/sessions", s.createSession)
			r.Post("/sessions/{id}/end", s.endSession)
			r.Get("/sessions/{id}/summary", s.sessionSummary)
			r.Get("/sessions/{id}/next", s.nextClip)
			r.Post("/feedback", s.postFeedback)
			r.Put("/ratings/{clip_id}", s.putRating)
			r.Get("/ratings", s.listRatings)
			// M3, educational: bytes through the API. Never mounted outside dev (API.md).
			if s.Cfg.AppEnv == "dev" {
				r.With(middleware.RateLimit(s.Redis, middleware.RulePlay, middleware.ByUser)).
					Get("/stream/{id}", s.streamClip)
			}
			// Admin: transcode, rollback (M5), status (M6), stats (M7). Never mounted in prod — production runs no
			// worker and Phase B is never deployed (decision 14), so a job there would only
			// ever sit queued. Per-user limit (step 7), then the admin check (step 8).
			if s.Cfg.AppEnv != "prod" {
				r.Group(func(r chi.Router) {
					r.Use(middleware.RateLimit(s.Redis, middleware.RuleAdmin, middleware.ByUser),
						middleware.RequireAdmin(s.Pool))
					r.Post("/admin/clips/{id}/transcode", s.createJob)
					r.Post("/admin/clips/{id}/delivery", s.setDelivery)
					r.Get("/admin/clips/{id}/status", s.clipStatus)
					r.Get("/admin/stats", s.adminStats)
				})
			}
		})
	})
	return r
}

// healthz answers without touching dependencies: it says the process serves HTTP, which is
// what a deploy check needs. A Postgres outage must not make Render restart a healthy API.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
