package middleware

import (
	"net/http"
	"strings"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// Auth resolves the caller (docs/API.md step 7). "jwt" verifies Authorization: Bearer with
// signer; "dev" injects the local dev user (config refuses it in prod); "closed" — and
// anything misconfigured — turns every user route away.
func Auth(mode, devUserID string, signer *auth.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var user string
			switch {
			case mode == "jwt" && signer != nil:
				tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
				if ok {
					user, _ = signer.Verify(tok) // "" on any failure
				}
			case mode == "dev":
				user = devUserID
			}
			if user == "" {
				httpx.Error(w, r, http.StatusUnauthorized, "unauthorized", "sign-in is required")
				return
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithUserID(r.Context(), user)))
		})
	}
}

// ByUser keys a per-user rule (step 8). It only runs after Auth, which guarantees a user.
func ByUser(r *http.Request) string {
	id, _ := httpx.UserID(r.Context())
	return id
}
