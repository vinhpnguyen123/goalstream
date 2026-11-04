package middleware

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

// RequireAdmin is docs/API.md step 9. It reads users.is_admin from Postgres on EVERY request
// instead of trusting a claim in the access token, so a demotion takes effect on the very next
// request rather than when the token expires. A caller who is not an admin — or no longer
// exists — gets the same 404 as a route that does not exist: admin routes are not discoverable.
func RequireAdmin(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, _ := httpx.UserID(r.Context()) // Auth ran first and guarantees one
			var admin bool
			err := pool.QueryRow(r.Context(), `SELECT is_admin FROM users WHERE id = $1`, user).Scan(&admin)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				httpx.Internal(w, r, err)
				return
			}
			if !admin {
				httpx.Error(w, r, http.StatusNotFound, "not_found", "not found")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
