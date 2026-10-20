package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertDevUser makes sure the AUTH_MODE=dev user exists and returns its id. password_hash
// '!' matches no argon2id hash, so the account can never log in with a password (M4).
// Config refuses AUTH_MODE=dev in prod, so this never runs there.
func UpsertDevUser(ctx context.Context, pool *pgxpool.Pool, email string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, is_admin)
		VALUES ($1, '!', 'Dev', true)
		ON CONFLICT (email) DO UPDATE SET password_hash = '!', is_admin = true
		RETURNING id`, email).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upserting dev user: %w", err)
	}
	return id, nil
}
