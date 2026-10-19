//go:build integration

package auth

import (
	"context"
	"os"
	"testing"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

func TestUpsertDevUserIsIdempotent(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users CASCADE`); err != nil {
		t.Fatal(err)
	}

	first, err := UpsertDevUser(ctx, pool, "dev@localhost")
	if err != nil {
		t.Fatal(err)
	}
	// Someone demoted the dev user and set a password: a restart restores the dev contract.
	if _, err := pool.Exec(ctx, `UPDATE users SET is_admin=false, password_hash='x' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	second, err := UpsertDevUser(ctx, pool, "dev@localhost")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("restart created a second dev user: %s then %s", first, second)
	}
	var admin bool
	var hash string
	if err := pool.QueryRow(ctx, `SELECT is_admin, password_hash FROM users WHERE id=$1`, first).Scan(&admin, &hash); err != nil {
		t.Fatal(err)
	}
	if !admin || hash != "!" {
		t.Fatalf("is_admin=%v password_hash=%q, want true and '!'", admin, hash)
	}
}
