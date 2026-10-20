package middleware

import (
	"net/http"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// Gate admits only requests carrying a valid invite cookie. Exemptions (healthz, POST /gate)
// are structural: those routes are mounted outside the group this middleware guards.
func Gate(c config.APIConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !c.GateEnabled {
			return next // refused in prod by config
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ck, err := r.Cookie(auth.InviteCookie)
			if err == nil {
				_, err = auth.VerifyInvite(c.InviteSecret, ck.Value, c.InviteCodes, time.Now())
			}
			if err != nil {
				httpx.Error(w, r, http.StatusUnauthorized, "gate_required", "an invite is required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
