//go:build integration

package auth

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, audit_log CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// Every login path runs exactly one argon2id comparison, so timing cannot tell an unknown
// email from a wrong password. Counted, not timed: a stopwatch test would be flaky.
func TestLoginAlwaysComparesOnce(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u, err := Register(ctx, pool, "me@x.com", "correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertDevUser(ctx, pool, "dev@localhost"); err != nil {
		t.Fatal(err)
	}

	calls := 0
	real := checkPassword
	checkPassword = func(pw, hash string) (bool, error) { calls++; return real(pw, hash) }
	t.Cleanup(func() { checkPassword = real })

	for _, c := range []struct {
		name, email, pw string
		wantID          string
		wantErr         error
	}{
		{"right password", "me@x.com", "correct horse battery", u.ID, nil},
		{"wrong password", "me@x.com", "wrong horse battery", "", ErrInvalidCredentials},
		{"unknown email", "nobody@x.com", "correct horse battery", "", ErrInvalidCredentials},
		{"account that cannot log in", "dev@localhost", "!", "", ErrInvalidCredentials},
	} {
		calls = 0
		id, err := Login(ctx, pool, c.email, c.pw)
		if id != c.wantID || !errors.Is(err, c.wantErr) {
			t.Errorf("%s: Login = %q, %v; want %q, %v", c.name, id, err, c.wantID, c.wantErr)
		}
		if calls != 1 {
			t.Errorf("%s: %d password comparisons, want exactly 1", c.name, calls)
		}
	}
}

func TestRegisterStoresHashAndAudits(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	name := "Vinh"
	u, err := Register(ctx, pool, "me@x.com", "correct horse battery", &name)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	var audits int
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1`, u.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log
		WHERE entity_type='user' AND entity_id=$1 AND action='user_registered' AND actor_type='user'`, u.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if hash[:10] != "$argon2id$" || audits != 1 {
		t.Fatalf("hash %.10q…, %d user_registered rows", hash, audits)
	}
	if _, err := Register(ctx, pool, "me@x.com", "another password", nil); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("second registration: %v, want ErrEmailTaken", err)
	}
}

func TestPruneRefresh(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u, err := Register(ctx, pool, "me@x.com", "correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	for tok, expires := range map[string]string{
		"old":     "now() - interval '2 days'", // expired > 1 day → pruned
		"recent":  "now() - interval '1 hour'", // expired < 1 day → kept
		"current": "now() + interval '7 days'", // active → kept
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at)
			VALUES ($1, $2, gen_random_uuid(), `+expires+`)`, u.ID, HashRefresh(tok)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := PruneRefresh(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("PruneRefresh = %d, %v; want 1", n, err)
	}
	var left int
	pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE token_hash IN ($1, $2)`,
		HashRefresh("recent"), HashRefresh("current")).Scan(&left)
	if left != 2 {
		t.Fatalf("%d of the two kept tokens remain", left)
	}
}

func TestSetAdmin(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	if _, err := Register(ctx, pool, "me@x.com", "correct horse battery", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		admin, want bool
	}{
		{"grant", true, true},
		{"grant again", true, false},
		{"revoke", false, true},
		{"revoke again", false, false},
	} {
		if changed, err := SetAdmin(ctx, pool, "me@x.com", c.admin); err != nil || changed != c.want {
			t.Errorf("%s: changed=%v, %v; want %v", c.name, changed, err, c.want)
		}
	}
	if _, err := SetAdmin(ctx, pool, "nobody@x.com", true); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown email: %v, want db.ErrNotFound", err)
	}
}
