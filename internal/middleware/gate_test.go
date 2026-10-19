package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
)

func TestGate(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	c := config.APIConfig{GateEnabled: true, InviteSecret: secret, InviteCodes: map[string]string{"me": "x"}}
	valid := auth.SignInvite(secret, "me", time.Now().Add(time.Hour))
	expired := auth.SignInvite(secret, "me", time.Now().Add(-time.Minute))
	revoked := auth.SignInvite(secret, "gone", time.Now().Add(time.Hour))

	for _, tc := range []struct {
		name, cookie string
		enabled      bool
		want         int
	}{
		{"no cookie", "", true, 401},
		{"valid", valid, true, 204},
		{"expired", expired, true, 401},
		{"revoked id", revoked, true, 401},
		{"garbage", "abc", true, 401},
		{"gate disabled", "", false, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.GateEnabled = tc.enabled
			req := httptest.NewRequest("GET", "/", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: auth.InviteCookie, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			Gate(c)(ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 401 && !strings.Contains(rec.Body.String(), `"gate_required"`) {
				t.Fatalf("body %s, want gate_required", rec.Body)
			}
		})
	}
}
