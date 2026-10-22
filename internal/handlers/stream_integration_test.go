//go:build integration

package handlers

import (
	"bytes"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stream sends GET /stream/{id} with an optional Range header. The body is bytes, not JSON.
func (e apiEnv) stream(id, rng string, withCookie bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/stream/"+id, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	if withCookie {
		req.AddCookie(e.cookie)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func devAPI(t *testing.T) apiEnv {
	e := setupAPI(t)
	e.s.Cfg.AppEnv = "dev"
	return apiEnv{e.s, e.s.Routes(), e.cookie}
}

// M3 Part 1: http.ServeContent is the target behaviour the hand-written handler must match.
func TestStreamServeContent(t *testing.T) {
	e := devAPI(t)
	body := bytes.Repeat([]byte("0123456789"), 1000)
	id := e.seedClip(t, "c", time.Now(), body)

	rec := e.stream(id, "", true)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), body) || rec.Header().Get("Accept-Ranges") != "bytes" ||
		rec.Header().Get("Content-Length") != strconv.Itoa(len(body)) || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("no Range: %d, %d bytes, headers %v", rec.Code, rec.Body.Len(), rec.Header())
	}
	rec = e.stream(id, "bytes=100-199", true)
	if rec.Code != 206 || rec.Header().Get("Content-Range") != "bytes 100-199/10000" ||
		!bytes.Equal(rec.Body.Bytes(), body[100:200]) {
		t.Fatalf("bytes=100-199: %d %q, %d bytes", rec.Code, rec.Header().Get("Content-Range"), rec.Body.Len())
	}
	if rec = e.stream(id, "bytes=10000-", true); rec.Code != 416 {
		t.Fatalf("start = size: %d, want 416", rec.Code)
	}
	for _, bad := range []string{uuid.NewString(), "not-a-uuid"} {
		if rec = e.stream(bad, "", true); rec.Code != 404 {
			t.Errorf("id %q: %d, want 404", bad, rec.Code)
		}
	}
	if rec = e.stream(id, "", false); rec.Code != 401 || !bytes.Contains(rec.Body.Bytes(), []byte("gate_required")) {
		t.Fatalf("no cookie: %d %s", rec.Code, rec.Body)
	}
}

// API.md: /stream exists only when APP_ENV=dev.
func TestStreamMountedOnlyInDev(t *testing.T) {
	e := setupAPI(t)
	id := e.seedClip(t, "c", time.Now(), []byte("x"))
	for _, env := range []string{"test", "prod"} {
		e.s.Cfg.AppEnv = env
		h := apiEnv{e.s, e.s.Routes(), e.cookie}
		if rec, body := h.get(t, "/api/v1/stream/"+id, true); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("APP_ENV=%s: %d %v, want 404 not_found", env, rec.Code, body)
		}
	}
}
