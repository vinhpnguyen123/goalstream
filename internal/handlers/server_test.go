package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vinhpnguyen123/goalstream/internal/middleware"
)

type envelope struct {
	Data      map[string]any `json:"data"`
	Error     *struct{ Type, Message string }
	RequestID string `json:"request_id"`
}

func do(t *testing.T, h http.Handler, method, path string, mod ...func(*http.Request)) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for _, m := range mod {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: body is not an envelope: %q", method, path, rec.Body.String())
	}
	return rec, env
}

func TestHealthzEnvelope(t *testing.T) {
	h := (&Server{}).Routes()
	rec, env := do(t, h, "GET", "/api/v1/healthz", func(r *http.Request) {
		r.Header.Set("X-Request-ID", "client-chosen")
	})
	if rec.Code != 200 || env.Data["status"] != "ok" {
		t.Fatalf("status %d, body %+v", rec.Code, env)
	}
	id := rec.Header().Get("X-Request-ID")
	if id == "" || id == "client-chosen" || env.RequestID != id {
		t.Fatalf("request id header %q, body %q: must be server-minted and match", id, env.RequestID)
	}
}

func TestUnknownRouteIsNotFoundEnvelope(t *testing.T) {
	h := (&Server{}).Routes()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/nope"},
		{"POST", "/api/v1/healthz"}, // wrong method: 404 too, the error set has no 405
		{"GET", "/healthz"},         // outside /api/v1
	} {
		rec, env := do(t, h, tc.method, tc.path)
		if rec.Code != 404 || env.Error == nil || env.Error.Type != "not_found" {
			t.Errorf("%s %s: %d %+v", tc.method, tc.path, rec.Code, env.Error)
		}
	}
}

func TestPanicBecomesInternal(t *testing.T) {
	h := middleware.RequestID(middleware.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec, env := do(t, h, "GET", "/")
	if rec.Code != 500 || env.Error == nil || env.Error.Type != "internal" || env.RequestID == "" {
		t.Fatalf("status %d, body %+v", rec.Code, env)
	}
	if env.Error.Message == "boom" {
		t.Fatal("panic value leaked to the client")
	}
}
