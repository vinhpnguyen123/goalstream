package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// ClipSummary is one row of GET /clips (docs/API.md). thumbnail_url arrives with
// clips.thumbnail_key in M5.
type ClipSummary struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	MatchMinute int       `json:"match_minute"`
	EventType   string    `json:"event_type"`
	SourceType  string    `json:"source_type"`
	CreatedAt   time.Time `json:"-"` // the cursor's half; not part of the response
}

// Cursor is the keyset position: the last row a page returned.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ListClips returns up to limit clips after the cursor, newest first. Keyset, not OFFSET: a
// page never repeats or skips a row when clips are ingested between requests.
func ListClips(ctx context.Context, pool *pgxpool.Pool, after *Cursor, limit int) ([]ClipSummary, error) {
	var at *time.Time
	var id *string
	if after != nil {
		at, id = &after.CreatedAt, &after.ID
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text, title, match_minute, event_type, source_type, created_at
		  FROM clips
		 WHERE $1::timestamptz IS NULL OR (created_at, id) < ($1, $2::uuid)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3`, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("listing clips: %w", err)
	}
	out := []ClipSummary{}
	var c ClipSummary
	_, err = pgx.ForEachRow(rows, []any{&c.ID, &c.Title, &c.MatchMinute, &c.EventType, &c.SourceType, &c.CreatedAt},
		func() error { out = append(out, c); return nil })
	if err != nil {
		return nil, fmt.Errorf("listing clips: %w", err)
	}
	return out, nil
}

// ClipPlayback returns what /play needs to build a URL. Missing → ErrNotFound.
func ClipPlayback(ctx context.Context, pool *pgxpool.Pool, id string) (prefix, sourceType string, err error) {
	err = pool.QueryRow(ctx, `SELECT s3_prefix, source_type FROM clips WHERE id = $1`, id).Scan(&prefix, &sourceType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("reading clip %s: %w", id, err)
	}
	return prefix, sourceType, nil
}
