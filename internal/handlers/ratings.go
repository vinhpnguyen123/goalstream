package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
)

// putRating is PUT /ratings/{clip_id}: re-rate (or rate) from the history screen, outside any
// session. Same function and responses as POST /feedback.
func (s *Server) putRating(w http.ResponseWriter, r *http.Request) {
	clip := chi.URLParam(r, "clip_id")
	if uuid.Validate(clip) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found") // malformed id = absent
		return
	}
	var req struct {
		Response *string `json:"response"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Response == nil || !validResponse(*req.Response) {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "response must be one of loved/okay/not_for_me")
		return
	}
	user, _ := httpx.UserID(r.Context())
	changed, prev, err := recommend.Rate(r.Context(), s.Pool, recommend.Rating{
		UserID: user, ClipID: clip, Response: *req.Response,
	})
	if !notFoundOr(w, r, err, "clip not found") {
		return
	}
	if !changed {
		httpx.JSON(w, r, http.StatusOK, map[string]any{"changed": false})
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"changed": true, "previous": prev})
}

// listRatings is GET /ratings?cursor=&limit=: the caller's current ratings, newest change first.
func (s *Server) listRatings(w http.ResponseWriter, r *http.Request) {
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}
	user, _ := httpx.UserID(r.Context())
	items, err := db.ListRatings(r.Context(), s.Pool, user, after, limit+1) // one extra: is there a next page?
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		c := encodeCursor(db.Cursor{CreatedAt: last.RatedAt, ID: last.ClipID})
		next = &c
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
