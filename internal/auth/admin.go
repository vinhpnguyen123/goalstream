package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

// SetAdmin grants (admin=true) or revokes is_admin for email — the only way it changes
// (`make admin`, plan §13 M4). Guarded: a user already in that state is changed=false.
// Unknown email → db.ErrNotFound. Not audited: there is no admin action in the closed audit
// vocabulary, and running this already requires direct database access.
func SetAdmin(ctx context.Context, pool *pgxpool.Pool, email string, admin bool) (bool, error) {
	tag, err := pool.Exec(ctx, `UPDATE users SET is_admin = $2 WHERE email = $1 AND is_admin <> $2`, email, admin)
	if err != nil {
		return false, fmt.Errorf("setting is_admin for %s: %w", email, err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return false, fmt.Errorf("reading user %s: %w", email, err)
	}
	if !exists {
		return false, db.ErrNotFound
	}
	return false, nil
}
