//go:build integration

package vocab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Keep the seeded tags; clear everything assigned at runtime.
	if _, err := pool.Exec(context.Background(), `DELETE FROM vocab WHERE kind <> 'tag'`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func slot(t *testing.T, pool *pgxpool.Pool, kind, value string) (int, error) {
	t.Helper()
	var s int
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		s, err = Slot(context.Background(), tx, kind, value)
		return err
	})
	return s, err
}

func TestSlotAssignmentIsStableAndPerKind(t *testing.T) {
	pool := setup(t)
	a, _ := slot(t, pool, "team", "Barcelona")
	b, _ := slot(t, pool, "team", "Real Madrid")
	again, _ := slot(t, pool, "team", " Barcelona ") // trimmed → same value
	c, _ := slot(t, pool, "competition", "spain_laliga")
	if a != 0 || b != 1 || again != 0 || c != 0 {
		t.Fatalf("slots: barca %d, real %d, barca again %d, laliga %d", a, b, again, c)
	}
	if _, err := slot(t, pool, "tag", "bicycle_kick"); err == nil {
		t.Fatal("runtime tag insertion must be refused")
	}
	tags, err := TagSlots(context.Background(), pool)
	if err != nil || len(tags) != 13 || tags["header"] != 0 || tags["winner"] != 12 {
		t.Fatalf("TagSlots = %v, %v", tags, err)
	}
}

func TestSlotExhaustionIsLoud(t *testing.T) {
	pool := setup(t)
	for i := 0; i < 64; i++ {
		if _, err := slot(t, pool, "team", fmt.Sprintf("team-%02d", i)); err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
	}
	_, err := slot(t, pool, "team", "one too many")
	if !errors.Is(err, ErrFull) {
		t.Fatalf("65th team: want ErrFull, got %v", err)
	}
}

// Two concurrent ingests must never hand out the same slot (run with -race, 3×).
func TestConcurrentAssignment(t *testing.T) {
	pool := setup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := slot(t, pool, "team", fmt.Sprintf("club-%d", i%20)) // every value requested twice
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n, distinct, maxSlot int
	pool.QueryRow(context.Background(),
		`SELECT count(*), count(DISTINCT slot), max(slot) FROM vocab WHERE kind='team'`).Scan(&n, &distinct, &maxSlot)
	if n != 20 || distinct != 20 || maxSlot != 19 {
		t.Fatalf("rows %d, distinct slots %d, max %d — want 20/20/19", n, distinct, maxSlot)
	}
}
