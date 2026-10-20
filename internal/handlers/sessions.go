package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// createSession is POST /sessions → {id, started_at, ended_at:null}.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	user, _ := httpx.UserID(r.Context()) // the auth middleware guarantees one
	sess, err := db.CreateSession(r.Context(), s.Pool, user)
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, sess)
}

// endSession is POST /sessions/{id}/end. Already ended → 200 with the original ended_at.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	user, _ := httpx.UserID(r.Context())
	sess, err := db.EndSession(r.Context(), s.Pool, id, user)
	if !notFoundOr(w, r, err, "session not found") {
		return
	}
	httpx.JSON(w, r, http.StatusOK, sess)
}

// sessionSummary is GET /sessions/{id}/summary.
func (s *Server) sessionSummary(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	user, _ := httpx.UserID(r.Context())
	sum, err := db.SessionSummary(r.Context(), s.Pool, id, user)
	if !notFoundOr(w, r, err, "session not found") {
		return
	}
	httpx.JSON(w, r, http.StatusOK, sum)
}

// sessionID reads {id}; a malformed id names no session → 404, like /clips/{id}/play.
func sessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if uuid.Validate(id) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "session not found")
		return "", false
	}
	return id, true
}

// notFoundOr answers the error cases (db.ErrNotFound → 404 with msg, anything else → 500) and
// reports whether the handler should go on.
func notFoundOr(w http.ResponseWriter, r *http.Request, err error, msg string) bool {
	if errors.Is(err, db.ErrNotFound) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", msg)
		return false
	}
	if err != nil {
		httpx.Internal(w, r, err)
		return false
	}
	return true
}
