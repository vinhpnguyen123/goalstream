package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// A mezzanine is ~60s at ≤ 8 Mbps (~61 MB); anything larger is not a clip.
const maxStreamBytes = 128 << 20

// streamClip is GET /stream/{id} — M3, EDUCATIONAL, mounted only when APP_ENV=dev. It is the
// one route that carries media bytes (principle #1's sole exception) and the player never
// uses it. It always serves the mp4 mezzanine, whatever source_type says.
//
// ponytail: Part 1 buffers the whole object so http.ServeContent can seek it — every request
// pulls the full file through the API, which is the cost of proxying made visible. Part 2
// (M3 S3) replaces this with a hand-written handler that fetches only the requested slice.
func (s *Server) streamClip(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if uuid.Validate(id) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	}
	prefix, _, err := db.ClipPlayback(r.Context(), s.Pool, id)
	if errors.Is(err, db.ErrNotFound) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	} else if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	// A row whose object is missing is an inconsistency, not an absent clip: Internal logs it.
	body, err := s.Store.Open(r.Context(), prefix+"original.mp4", "")
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	defer body.Close()
	buf, err := io.ReadAll(io.LimitReader(body, maxStreamBytes+1))
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	if len(buf) > maxStreamBytes {
		httpx.Internal(w, r, fmt.Errorf("clip %s exceeds %d bytes", id, maxStreamBytes))
		return
	}
	http.ServeContent(w, r, "original.mp4", time.Time{}, bytes.NewReader(buf))
}
