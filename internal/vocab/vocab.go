// Package vocab assigns stable embedding slots to categorical values (team, competition,
// season, tag). Values come from the data; the code never knows a team or league name.
package vocab

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrFull means a kind ran out of slots (capacity is a CHECK constraint in the schema).
// It is a configuration problem — embedding v2 is needed — so callers abort, never skip.
var ErrFull = errors.New("vocab: no free slot")

// Slot returns value's slot for kind, assigning the next free one if the value is new.
// It must run inside a transaction: the advisory lock is transaction-scoped and serializes
// assignment per kind, so two concurrent ingests can never hand out the same slot.
func Slot(ctx context.Context, tx pgx.Tx, kind, value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("vocab: empty %s value", kind)
	}
	if kind == "tag" {
		return 0, fmt.Errorf("vocab: tags are seeded by migration, never assigned at runtime")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('vocab:' || $1))`, kind); err != nil {
		return 0, fmt.Errorf("locking vocab %s: %w", kind, err)
	}
	var slot int
	err := tx.QueryRow(ctx, `SELECT slot FROM vocab WHERE kind=$1 AND value=$2`, kind, value).Scan(&slot)
	if err == nil {
		return slot, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("reading vocab %s %q: %w", kind, value, err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO vocab (kind, value, slot)
		SELECT $1, $2, COALESCE(MAX(slot) + 1, 0) FROM vocab WHERE kind = $1
		RETURNING slot`, kind, value).Scan(&slot)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "vocab_slot_chk" {
		return 0, fmt.Errorf("%w for kind %q (value %q): embedding v2 needed", ErrFull, kind, value)
	}
	if err != nil {
		return 0, fmt.Errorf("assigning vocab %s %q: %w", kind, value, err)
	}
	return slot, nil
}

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TagSlots returns the seeded tag vocabulary: tag → slot.
func TagSlots(ctx context.Context, q Querier) (map[string]int, error) {
	rows, err := q.Query(ctx, `SELECT value, slot FROM vocab WHERE kind='tag'`)
	if err != nil {
		return nil, fmt.Errorf("reading tag vocab: %w", err)
	}
	out := map[string]int{}
	var v string
	var s int
	_, err = pgx.ForEachRow(rows, []any{&v, &s}, func() error { out[v] = s; return nil })
	if err != nil {
		return nil, fmt.Errorf("reading tag vocab: %w", err)
	}
	return out, nil
}
