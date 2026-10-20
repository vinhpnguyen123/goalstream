package handlers

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
)

const maxExclude = 100

// nextClip is GET /sessions/{id}/next?exclude=<uuid,…>. An ended session is still served,
// like feedback.
func (s *Server) nextClip(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	exclude, ok := parseExclude(r.URL.Query().Get("exclude"))
	if !ok {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "exclude must be at most 100 comma-separated clip ids")
		return
	}
	user, _ := httpx.UserID(r.Context())
	if !notFoundOr(w, r, db.OwnsSession(r.Context(), s.Pool, id, user), "session not found") {
		return
	}
	pick, err := recommend.Next(r.Context(), s.Pool, user, exclude)
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, pick)
}

// parseExclude reads a comma-separated list of UUIDs. Empty is no exclusions; an empty
// element, a malformed id or more than maxExclude is invalid.
func parseExclude(raw string) ([]string, bool) {
	if raw == "" {
		return []string{}, true
	}
	ids := strings.Split(raw, ",")
	if len(ids) > maxExclude {
		return nil, false
	}
	for _, id := range ids {
		if uuid.Validate(id) != nil {
			return nil, false
		}
	}
	return ids, true
}
