package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentRating is one row of GET /ratings: the user's latest response to a clip.
type CurrentRating struct {
	ClipID   string    `json:"clip_id"`
	Title    string    `json:"title"`
	Response string    `json:"response"`
	RatedAt  time.Time `json:"rated_at"`
}

// ListRatings returns up to limit of the user's current ratings after the cursor, newest change
// first. feedback_events is append-only, so "current" is the latest row per clip — DISTINCT ON
// walks feedback_latest_idx. Keyset over (rated_at, clip_id): Cursor.CreatedAt is rated_at.
// ponytail: computes every current rating before paging; fine at a few hundred ratings per user.
func ListRatings(ctx context.Context, pool *pgxpool.Pool, userID string, after *Cursor, limit int) ([]CurrentRating, error) {
	var at *time.Time
	var id *string
	if after != nil {
		at, id = &after.CreatedAt, &after.ID
	}
	rows, err := pool.Query(ctx, `
		SELECT cur.clip_id::text, c.title, cur.response, cur.rated_at
		  FROM (SELECT DISTINCT ON (clip_id) clip_id, response, created_at AS rated_at
		          FROM feedback_events
		         WHERE user_id = $1
		         ORDER BY clip_id, created_at DESC, id DESC) cur
		  JOIN clips c ON c.id = cur.clip_id
		 WHERE $2::timestamptz IS NULL OR (cur.rated_at, cur.clip_id) < ($2, $3::uuid)
		 ORDER BY cur.rated_at DESC, cur.clip_id DESC
		 LIMIT $4`, userID, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("listing ratings: %w", err)
	}
	out := []CurrentRating{}
	var r CurrentRating
	_, err = pgx.ForEachRow(rows, []any{&r.ClipID, &r.Title, &r.Response, &r.RatedAt},
		func() error { out = append(out, r); return nil })
	if err != nil {
		return nil, fmt.Errorf("listing ratings: %w", err)
	}
	return out, nil
}
