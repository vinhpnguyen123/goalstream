package handlers

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
)

type feedbackRequest struct {
	SessionID   *string `json:"session_id"`
	ClipID      *string `json:"clip_id"`
	Response    *string `json:"response"`
	ReplayCount *int    `json:"replay_count"`
	WatchedFull *bool   `json:"watched_full"`
}

// postFeedback is POST /feedback: rate or re-rate a clip inside a session.
// → {changed:false} when the response equals the current rating, else {changed:true, previous}.
func (s *Server) postFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackRequest
	if !decode(w, r, &req) {
		return
	}
	if req.SessionID == nil || uuid.Validate(*req.SessionID) != nil ||
		req.ClipID == nil || uuid.Validate(*req.ClipID) != nil ||
		req.Response == nil || !validResponse(*req.Response) ||
		req.ReplayCount == nil || *req.ReplayCount < 0 ||
		req.WatchedFull == nil {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error",
			"session_id and clip_id must be UUIDs, response one of loved/okay/not_for_me, replay_count ≥ 0, watched_full a boolean")
		return
	}
	user, _ := httpx.UserID(r.Context())
	changed, prev, err := recommend.Rate(r.Context(), s.Pool, recommend.Rating{
		SessionID: req.SessionID, UserID: user, ClipID: *req.ClipID, Response: *req.Response,
		ReplayCount: *req.ReplayCount, WatchedFull: req.WatchedFull,
	})
	if !notFoundOr(w, r, err, "session or clip not found") {
		return
	}
	if !changed {
		httpx.JSON(w, r, http.StatusOK, map[string]any{"changed": false})
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"changed": true, "previous": prev})
}

func validResponse(s string) bool {
	return s == "loved" || s == "okay" || s == "not_for_me"
}
