package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

func TestAuthModes(t *testing.T) {
	var seen string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = httpx.UserID(r.Context())
		w.WriteHeader(204)
	})
	for _, tc := range []struct {
		name, mode, dev string
		want            int
	}{
		{"closed", "closed", "", 401},
		{"closed ignores a dev id", "closed", "u1", 401},
		{"dev", "dev", "u1", 204},
		{"dev without a user", "dev", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = ""
			rec := httptest.NewRecorder()
			Auth(tc.mode, tc.dev)(h).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 401 && !strings.Contains(rec.Body.String(), `"unauthorized"`) {
				t.Fatalf("body %s", rec.Body)
			}
			if tc.want == 204 && seen != tc.dev {
				t.Fatalf("handler saw user %q, want %q", seen, tc.dev)
			}
		})
	}
}
