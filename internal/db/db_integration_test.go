//go:build integration

package db

import (
	"context"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/vinhpnguyen123/goalstream/migrations"
)

func testURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Fatal("TEST_DATABASE_URL is not set")
	}
	return u
}

func TestMigrateIsIdempotentAndSeedsTags(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)

	// Twice: the second run must be a no-op (ErrNoChange is not an error).
	for i := 0; i < 2; i++ {
		if err := Migrate(url); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}

	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var tables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public'
		   AND table_name IN ('matches','vocab','clips','users','audit_log')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 5 {
		t.Fatalf("want 5 M1 tables, got %d", tables)
	}

	var tags, maxSlot int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(slot) FROM vocab WHERE kind='tag'`).Scan(&tags, &maxSlot); err != nil {
		t.Fatal(err)
	}
	if tags != 13 || maxSlot != 12 {
		t.Fatalf("want 13 seeded tags in slots 0..12, got %d (max slot %d)", tags, maxSlot)
	}

	// Capacity is enforced by the database: team slot 64 does not exist.
	_, err = pool.Exec(ctx, `INSERT INTO vocab (kind, value, slot) VALUES ('team','__overflow__',64)`)
	if err == nil {
		pool.Exec(ctx, `DELETE FROM vocab WHERE value='__overflow__'`)
		t.Fatal("team slot 64 was accepted; vocab_slot_chk must reject it")
	}
}

// Down to M1 and back up: the M2 tables go and return cleanly.
func TestMigrateDownToM1AndUp(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(url); err != nil {
		t.Fatal(err)
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, pgx5URL(url))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m2Tables := func() (n int) {
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			 WHERE table_schema='public' AND table_name IN ('sessions','feedback_events','seen_clips')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := m.Migrate(1); err != nil {
		t.Fatalf("down to 1: %v", err)
	}
	if n := m2Tables(); n != 0 {
		t.Fatalf("after down: %d M2 tables remain", n)
	}
	if err := Migrate(url); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := m2Tables(); n != 3 {
		t.Fatalf("after up: %d M2 tables, want 3", n)
	}
}
