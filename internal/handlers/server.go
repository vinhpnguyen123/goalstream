// Package handlers holds the HTTP routes. Handlers stay thin: decode → internal/ → encode.
package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/middleware"
)

type Server struct {
	Pool *pgxpool.Pool
	Cfg  config.Config
}

// Routes builds the router. Middleware order is docs/API.md § Middleware order; every route
// lives under /api/v1.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recover, middleware.LimitBody)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "not found") // no 405: the set is closed
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/healthz", s.healthz)
	})
	return r
}

// healthz answers without touching dependencies: it says the process serves HTTP, which is
// what a deploy check needs. A Postgres outage must not make Render restart a healthy API.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
