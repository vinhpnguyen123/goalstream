package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// One line per request, joined to X-Request-ID, naming the route pattern and the real status —
// a panic's 500 included — and never the query string or a header value.
func TestRequestLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := chi.NewRouter()
	r.Use(RequestID, RequestLog, Recover)
	r.Get("/clips/{id}/play", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	r.Get("/quiet", func(http.ResponseWriter, *http.Request) {})

	type line struct {
		Msg, RequestID, Method, Route string
		Status                        int
	}
	do := func(path string) (string, line) {
		t.Helper()
		buf.Reset()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "gs_invite=SECRETCOOKIE")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var got line
		for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("not JSON: %s", raw)
			}
			if m["msg"] == "request" {
				status, _ := m["status"].(float64)
				got = line{"request", str(m["request_id"]), str(m["method"]), str(m["route"]), int(status)}
			}
		}
		if strings.Contains(buf.String(), "SECRET") {
			t.Errorf("%s: log carries a query string or cookie: %s", path, buf.String())
		}
		return rec.Header().Get("X-Request-ID"), got
	}

	id, l := do("/clips/abc/play?cursor=SECRETCURSOR")
	if l != (line{"request", id, "GET", "/clips/{id}/play", http.StatusTeapot}) || id == "" {
		t.Errorf("play: %+v, want request_id %s, route pattern, 418", l, id)
	}
	if id, l = do("/boom"); l.Status != 500 || l.RequestID != id {
		t.Errorf("panic: %+v, want 500 with request_id %s", l, id)
	}
	if _, l = do("/quiet"); l.Status != 200 {
		t.Errorf("handler that writes nothing: %+v, want 200", l)
	}
	if _, l = do("/nope"); l.Route != "unmatched" || l.Status != 404 {
		t.Errorf("unknown path: %+v, want route unmatched, 404", l)
	}
}

func str(v any) string { s, _ := v.(string); return s }
