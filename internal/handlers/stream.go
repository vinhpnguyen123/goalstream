package handlers

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// streamClip is GET /stream/{id} — M3, EDUCATIONAL, mounted only when APP_ENV=dev. It is the
// one route that carries media bytes (principle #1's sole exception) and the player never
// uses it. It always serves the mp4 mezzanine, whatever source_type says.
//
// Hand-written Range support: HEAD for the size, parse, then fetch ONLY the requested slice
// from storage (GetObjectInput.Range). The original is never mutated, so the size cannot
// change between the HEAD and the GET.
func (s *Server) streamClip(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if uuid.Validate(id) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	}
	pb, err := db.ClipPlayback(r.Context(), s.Pool, id)
	if errors.Is(err, db.ErrNotFound) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found")
		return
	} else if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	key := pb.Prefix + "original.mp4"
	// A row whose object is missing is an inconsistency, not an absent clip: Internal logs it.
	size, err := s.Store.Size(r.Context(), key)
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	rng, err := httpx.ParseRange(r.Header.Get("Range"), size)
	if err != nil {
		// RFC 9110 §15.5.17: say how big the body is. No envelope — this route serves media.
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	status, length, storageRange := http.StatusOK, size, ""
	if rng != nil {
		status, length = http.StatusPartialContent, rng.Len()
		storageRange = "bytes=" + strconv.FormatInt(rng.Start, 10) + "-" + strconv.FormatInt(rng.End, 10)
		h.Set("Content-Range", rng.ContentRange(size))
	}
	body, err := s.Store.Open(r.Context(), key, storageRange)
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	defer body.Close()

	h.Set("Content-Type", "video/mp4")
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if _, err := io.Copy(w, body); err != nil {
		// Headers are already sent; the status cannot change. Usually the client went away.
		httpx.Log(r.Context()).Warn("stream copy interrupted", "clip_id", id, "err", err)
	}
}
