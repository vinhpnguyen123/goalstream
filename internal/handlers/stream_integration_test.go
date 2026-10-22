//go:build integration

package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

var streamBody = bytes.Repeat([]byte("0123456789"), 1000) // 10,000 bytes

// oracle is http.ServeContent over the same bytes: the stdlib behaviour the hand-written
// handler must reproduce (M3 Part 1 → Part 2).
func oracle(rng string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/original.mp4", nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, "original.mp4", time.Time{}, bytes.NewReader(streamBody))
	return rec
}

// §12 #10: wherever the plan does not ask for something else, the handler answers exactly
// like http.ServeContent — status, Content-Range, Content-Length and body.
func TestStreamMatchesServeContent(t *testing.T) {
	e := devAPI(t)
	id := e.seedClip(t, "c", time.Now(), streamBody)
	for _, rng := range []string{
		"", "bytes=0-99", "bytes=100-", "bytes=-100", "bytes=0-0", "bytes=9990-20000", "bytes=-20000",
		"bytes=10000-", "bytes=5-2", "bytes=abc", "items=0-1",
	} {
		got, want := e.stream(id, rng, true), oracle(rng)
		if got.Code != want.Code {
			t.Errorf("%q: status %d, ServeContent %d", rng, got.Code, want.Code)
			continue
		}
		if got.Code == 416 {
			continue // ServeContent writes a text body; ours is empty (TestStreamDeviations)
		}
		for _, h := range []string{"Content-Range", "Content-Length", "Content-Type", "Accept-Ranges"} {
			if g, w := got.Header().Get(h), want.Header().Get(h); g != w {
				t.Errorf("%q: %s %q, ServeContent %q", rng, h, g, w)
			}
		}
		if !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
			t.Errorf("%q: %d body bytes differ from ServeContent's %d", rng, got.Body.Len(), want.Body.Len())
		}
	}
}

// Where the plan or RFC 9110 is stricter than ServeContent, the expectation is written by hand.
func TestStreamDeviations(t *testing.T) {
	e := devAPI(t)
	id := e.seedClip(t, "c", time.Now(), streamBody)

	// Multi-range is ignored: 200 with the whole body (ServeContent: 206 multipart).
	rec := e.stream(id, "bytes=0-1,5-6", true)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), streamBody) || rec.Header().Get("Content-Range") != "" {
		t.Errorf("multi-range: %d, %d bytes, Content-Range %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Range"))
	}
	// Every 416 states the size and has an empty body — including malformed headers, where
	// ServeContent omits Content-Range, and "-0" / "+1", which ServeContent accepts.
	for _, rng := range []string{"bytes=10000-", "bytes=abc", "bytes=-0", "bytes=+1-2"} {
		rec := e.stream(id, rng, true)
		if rec.Code != 416 || rec.Header().Get("Content-Range") != "bytes */10000" || rec.Body.Len() != 0 {
			t.Errorf("%q: %d, Content-Range %q, %d body bytes", rng, rec.Code, rec.Header().Get("Content-Range"), rec.Body.Len())
		}
	}
}

func TestStreamNotFoundAndGate(t *testing.T) {
	e := devAPI(t)
	id := e.seedClip(t, "c", time.Now(), streamBody)
	for _, bad := range []string{uuid.NewString(), "not-a-uuid"} {
		if rec := e.stream(bad, "", true); rec.Code != 404 {
			t.Errorf("id %q: %d, want 404", bad, rec.Code)
		}
	}
	if rec := e.stream(id, "", false); rec.Code != 401 || !bytes.Contains(rec.Body.Bytes(), []byte("gate_required")) {
		t.Errorf("no cookie: %d %s", rec.Code, rec.Body)
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
