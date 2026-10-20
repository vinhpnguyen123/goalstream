package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session is one viewing session. Every lookup is scoped by user: a session someone else owns
// is ErrNotFound, never "forbidden" (docs/API.md).
type Session struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

// Summary is GET /sessions/{id}/summary. Each clip counts once, by its latest rating in this
// session: a re-rate inside the session moves the clip between buckets, it does not add one.
type Summary struct {
	Rated     int        `json:"rated"`
	Loved     int        `json:"loved"`
	Okay      int        `json:"okay"`
	NotForMe  int        `json:"not_for_me"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

func CreateSession(ctx context.Context, pool *pgxpool.Pool, userID string) (Session, error) {
	var s Session
	err := pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id) VALUES ($1)
		RETURNING id::text, started_at, ended_at`, userID).Scan(&s.ID, &s.StartedAt, &s.EndedAt)
	if err != nil {
		return Session{}, fmt.Errorf("creating session: %w", err)
	}
	return s, nil
}

// EndSession sets ended_at once. The UPDATE is guarded on ended_at IS NULL, so a second call
// changes nothing and returns the session as it already was; 0 rows on both statements means
// the session is absent or not the caller's.
func EndSession(ctx context.Context, pool *pgxpool.Pool, id, userID string) (Session, error) {
	s := Session{ID: id}
	err := pool.QueryRow(ctx, `
		UPDATE sessions SET ended_at = now()
		 WHERE id = $1 AND user_id = $2 AND ended_at IS NULL
		RETURNING started_at, ended_at`, id, userID).Scan(&s.StartedAt, &s.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = pool.QueryRow(ctx, `SELECT started_at, ended_at FROM sessions WHERE id = $1 AND user_id = $2`,
			id, userID).Scan(&s.StartedAt, &s.EndedAt)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("ending session %s: %w", id, err)
	}
	return s, nil
}

func SessionSummary(ctx context.Context, pool *pgxpool.Pool, id, userID string) (Summary, error) {
	var s Summary
	err := pool.QueryRow(ctx, `
		WITH latest AS (
		  SELECT DISTINCT ON (f.clip_id) f.response
		    FROM feedback_events f
		   WHERE f.session_id = $1
		   ORDER BY f.clip_id, f.created_at DESC, f.id DESC
		)
		SELECT s.started_at, s.ended_at,
		       (SELECT count(*) FROM latest),
		       (SELECT count(*) FROM latest WHERE response = 'loved'),
		       (SELECT count(*) FROM latest WHERE response = 'okay'),
		       (SELECT count(*) FROM latest WHERE response = 'not_for_me')
		  FROM sessions s
		 WHERE s.id = $1 AND s.user_id = $2`, id, userID).
		Scan(&s.StartedAt, &s.EndedAt, &s.Rated, &s.Loved, &s.Okay, &s.NotForMe)
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, fmt.Errorf("summarising session %s: %w", id, err)
	}
	return s, nil
}

// OwnsSession is ErrNotFound unless the session exists and belongs to userID.
func OwnsSession(ctx context.Context, pool *pgxpool.Pool, id, userID string) error {
	var ok bool
	err := pool.QueryRow(ctx, `SELECT true FROM sessions WHERE id = $1 AND user_id = $2`, id, userID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("reading session %s: %w", id, err)
	}
	return nil
}
