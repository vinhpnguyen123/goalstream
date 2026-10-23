package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

func TestAuthModes(t *testing.T) {
	signer := auth.NewSigner(strings.Repeat("k", 32), 15*time.Minute)
	user := uuid.NewString()
	good, _, _ := signer.Issue(user, time.Now())
	expired, _, _ := signer.Issue(user, time.Now().Add(-time.Hour))
	foreign, _, _ := auth.NewSigner(strings.Repeat("z", 32), 15*time.Minute).Issue(user, time.Now())

	var seen string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = httpx.UserID(r.Context())
		w.WriteHeader(204)
	})
	for _, tc := range []struct {
		name, mode, dev, authz string
		want                   int
		wantUser               string
	}{
		{"closed", "closed", "", "", 401, ""},
		{"closed ignores a dev id", "closed", "u1", "", 401, ""},
		{"closed ignores a valid token", "closed", "", "Bearer " + good, 401, ""},
		{"dev", "dev", "u1", "", 204, "u1"},
		{"dev without a user", "dev", "", "", 401, ""},
		{"jwt", "jwt", "", "Bearer " + good, 204, user},
		{"jwt ignores the dev id", "jwt", "u1", "", 401, ""},
		{"jwt no header", "jwt", "", "", 401, ""},
		{"jwt no Bearer prefix", "jwt", "", good, 401, ""},
		{"jwt lowercase scheme", "jwt", "", "bearer " + good, 401, ""},
		{"jwt expired", "jwt", "", "Bearer " + expired, 401, ""},
		{"jwt other secret", "jwt", "", "Bearer " + foreign, 401, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = ""
			req := httptest.NewRequest("GET", "/", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()
			Auth(tc.mode, tc.dev, signer)(h).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 401 && !strings.Contains(rec.Body.String(), `"unauthorized"`) {
				t.Fatalf("body %s", rec.Body)
			}
			if tc.want == 204 && seen != tc.wantUser {
				t.Fatalf("handler saw user %q, want %q", seen, tc.wantUser)
			}
		})
	}
}
