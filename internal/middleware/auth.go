package middleware

import (
	"net/http"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// Auth resolves the caller (docs/API.md step 6). "closed" turns every user route away —
// production until M4. "dev" injects the local dev user; config refuses it in prod.
// "jwt" arrives in M4.
func Auth(mode, devUserID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode != "dev" || devUserID == "" {
				httpx.Error(w, r, http.StatusUnauthorized, "unauthorized", "sign-in is required")
				return
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithUserID(r.Context(), devUserID)))
		})
	}
}

// ByUser keys a per-user rule (step 7). It only runs after Auth, which guarantees a user.
func ByUser(r *http.Request) string {
	id, _ := httpx.UserID(r.Context())
	return id
}
