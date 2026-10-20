package recommend

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

// Rating is one rate or re-rate. SessionID nil = re-rated outside a session (PUT /ratings, M4),
// and then WatchedFull is nil too (feedback_session_chk).
type Rating struct {
	SessionID   *string
	UserID      string
	ClipID      string
	Response    string // loved | okay | not_for_me
	ReplayCount int
	WatchedFull *bool
}

// Rate is the one function behind POST /feedback and PUT /ratings (plan §11). It is
// idempotent by state: a response equal to the caller's current rating of the clip changes
// nothing, so double taps and client retries are no-ops. A session that is not the caller's,
// or a clip that does not exist, is db.ErrNotFound.
//
// M2 has no centroids: a change appends the event and marks the clip seen, nothing else. M4
// inserts the subtract-then-add between reading prev and appending.
func Rate(ctx context.Context, pool *pgxpool.Pool, in Rating) (changed bool, previous *string, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Taken FIRST: every rating change of one user is serialized, so two concurrent
		// requests cannot both read the same prev and both append. NO KEY so INSERTs that
		// reference this user (FOR KEY SHARE) are not blocked. ponytail: M4 moves this to
		// user_taste … FOR UPDATE, the row the centroid update needs anyway.
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1 FOR NO KEY UPDATE`, in.UserID).Scan(&ok); err != nil {
			return fmt.Errorf("locking user %s: %w", in.UserID, err)
		}
		if in.SessionID != nil {
			// An ended session still accepts feedback: a retry may land after /end.
			err := tx.QueryRow(ctx, `SELECT true FROM sessions WHERE id = $1 AND user_id = $2`,
				*in.SessionID, in.UserID).Scan(&ok)
			if errors.Is(err, pgx.ErrNoRows) {
				return db.ErrNotFound
			} else if err != nil {
				return fmt.Errorf("reading session %s: %w", *in.SessionID, err)
			}
		}
		err := tx.QueryRow(ctx, `SELECT true FROM clips WHERE id = $1`, in.ClipID).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("reading clip %s: %w", in.ClipID, err)
		}

		err = tx.QueryRow(ctx, `
			SELECT response FROM feedback_events
			 WHERE user_id = $1 AND clip_id = $2
			 ORDER BY created_at DESC, id DESC
			 LIMIT 1`, in.UserID, in.ClipID).Scan(&previous)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("reading current rating: %w", err)
		}
		if previous != nil && *previous == in.Response {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO feedback_events (session_id, user_id, clip_id, response, replay_count, watched_full)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			in.SessionID, in.UserID, in.ClipID, in.Response, in.ReplayCount, in.WatchedFull); err != nil {
			return fmt.Errorf("appending feedback: %w", err)
		}
		// Never removed: a re-rated clip is still seen.
		if _, err := tx.Exec(ctx, `
			INSERT INTO seen_clips (user_id, clip_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			in.UserID, in.ClipID); err != nil {
			return fmt.Errorf("marking clip seen: %w", err)
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return changed, previous, nil
}
