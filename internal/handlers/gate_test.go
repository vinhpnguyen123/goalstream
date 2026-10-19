package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func gateConfig() config.APIConfig {
	return config.APIConfig{GateEnabled: true, CookieSecure: true, InviteTTL: time.Hour,
		InviteCodes: map[string]string{"me": "let-me-in"}, InviteSecret: testSecret}
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

// §12 #12 (second half): the gate is brute-force protection, so with Redis down it refuses.
func TestGateFailsClosedWithoutRedis(t *testing.T) {
	s := testServer() // dead Redis
	s.Cfg.API = gateConfig()
	rec := post(s.Routes(), "/api/v1/gate", `{"code":"let-me-in"}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"unavailable"`) {
		t.Fatalf("got %d %s, want 503 unavailable", rec.Code, rec.Body)
	}
}

func TestGateHandler(t *testing.T) {
	s := &Server{Cfg: config.Config{API: gateConfig()}}
	cases := []struct {
		name, body, wantType string
		want                 int
	}{
		{"right code", `{"code":"let-me-in"}`, "", 204},
		{"wrong code", `{"code":"nope"}`, "gate_required", 401},
		{"malformed", `{"code":`, "validation_error", 400},
		{"unknown field", `{"code":"let-me-in","admin":true}`, "validation_error", 400},
		{"too large", `{"code":"` + strings.Repeat("a", 70<<10) + `"}`, "payload_too_large", 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Handler(http.HandlerFunc(s.gate))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/v1/gate", strings.NewReader(tc.body))
			req.Body = http.MaxBytesReader(rec, req.Body, 64<<10)
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.wantType) {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
			if tc.want != 204 {
				if rec.Header().Get("Set-Cookie") != "" {
					t.Fatal("cookie set on a failed attempt")
				}
				return
			}
			c := rec.Result().Cookies()[0]
			if c.Name != auth.InviteCookie || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode ||
				c.Path != "/" || c.Domain != "" || c.MaxAge != 3600 {
				t.Fatalf("cookie attributes wrong: %+v", c)
			}
			if id, err := auth.VerifyInvite(testSecret, c.Value, gateConfig().InviteCodes, time.Now()); err != nil || id != "me" {
				t.Fatalf("issued cookie does not verify: %v", err)
			}
		})
	}
}
