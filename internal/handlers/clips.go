package handlers

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// pageParams reads ?limit= (1–100, default 20) and ?cursor= for the keyset-paged lists,
// answering 400 itself when either is invalid.
func pageParams(w http.ResponseWriter, r *http.Request) (limit int, after *db.Cursor, ok bool) {
	limit = defaultPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageSize {
			httpx.Error(w, r, http.StatusBadRequest, "validation_error", "limit must be an integer from 1 to 100")
			return 0, nil, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			httpx.Error(w, r, http.StatusBadRequest, "validation_error", "cursor is invalid")
			return 0, nil, false
		}
		after = &c
	}
	return limit, after, true
}

// listClips is GET /clips?cursor=&limit=.
func (s *Server) listClips(w http.ResponseWriter, r *http.Request) {
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}

	items, err := db.ListClips(r.Context(), s.Pool, after, limit+1) // one extra: is there a next page?
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		c := encodeCursor(db.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		next = &c
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// playClip is GET /clips/{id}/play?format=. It returns a presigned URL and nothing else: the
// browser fetches the bytes from storage directly (principle #1). It does not write
// seen_clips — only feedback does.
func (s *Server) playClip(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("format") {
	case "", "mp4":
	default:
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "format must be mp4 or omitted")
		return
	}
	id := chi.URLParam(r, "id")
	if uuid.Validate(id) != nil {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "clip not found") // a malformed id names no clip
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
	// ponytail: M1 has only mp4 clips; M5 adds the hls branch on source_type + hls_version.
	url, exp, err := s.Store.PresignGet(r.Context(), prefix+"original.mp4")
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"type": "mp4", "url": url, "expires_at": exp.UTC()})
}

// The cursor is opaque to clients: base64url(created_at RFC3339Nano | id).
func encodeCursor(c db.Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

func decodeCursor(s string) (db.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return db.Cursor{}, err
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || uuid.Validate(id) != nil {
		return db.Cursor{}, errors.New("malformed cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return db.Cursor{}, err
	}
	return db.Cursor{CreatedAt: at, ID: id}, nil
}
