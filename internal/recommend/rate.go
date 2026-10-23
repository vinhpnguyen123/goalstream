package recommend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

// Rating is one rate or re-rate. SessionID nil = re-rated outside a session (PUT /ratings),
// and then WatchedFull is nil too (feedback_session_chk).
type Rating struct {
	SessionID   *string
	UserID      string
	ClipID      string
	Response    string // loved | okay | not_for_me
	ReplayCount int
	WatchedFull *bool
}

// taste is the caller's user_taste row: two running means and their counts.
type taste struct {
	pos, neg        []float32
	liked, disliked int
}

// move applies one rating transition (plan §11 steps 4–5): remove the clip's vector v from
// prev's centroid, then add it to next's. 'okay' (and no previous rating, "") has no centroid.
func (t *taste) move(prev, next string, v []float32) {
	switch prev {
	case "loved":
		t.pos, t.liked = removeVec(t.pos, t.liked, v), t.liked-1
	case "not_for_me":
		t.neg, t.disliked = removeVec(t.neg, t.disliked, v), t.disliked-1
	}
	switch next {
	case "loved":
		t.pos, t.liked = addVec(t.pos, t.liked, v), t.liked+1
	case "not_for_me":
		t.neg, t.disliked = addVec(t.neg, t.disliked, v), t.disliked+1
	}
}

// Rate is the one function behind POST /feedback and PUT /ratings (plan §11). It is
// idempotent by state: a response equal to the caller's current rating of the clip changes
// nothing, so double taps and client retries are no-ops. A session that is not the caller's,
// or a clip that does not exist, is db.ErrNotFound. A change appends the event, marks the clip
// seen and moves the clip's vector between the caller's centroids, all in one transaction.
func Rate(ctx context.Context, pool *pgxpool.Pool, in Rating) (changed bool, previous *string, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRebuildShared(ctx, tx); err != nil {
			return err
		}
		// Taken next, before anything is read: every rating change of one user is serialized,
		// so two concurrent requests cannot both read the same prev and both move the vector.
		tag, err := tx.Exec(ctx, `INSERT INTO user_taste (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, in.UserID)
		if err != nil {
			return fmt.Errorf("creating taste of %s: %w", in.UserID, err)
		}
		// A new row may still have history: ratings from before user_taste existed (M2). Build
		// it from them now, or the first re-rate of an old clip would subtract a vector the
		// empty row never held (count −1 → taste_*_chk → 500). A concurrent first Rate waits on
		// our uncommitted row, then finds it and skips this.
		if tag.RowsAffected() == 1 {
			if err := rebuildTaste(ctx, tx, &in.UserID); err != nil {
				return err
			}
		}
		var t taste
		var pos, neg *string
		if err := tx.QueryRow(ctx, `
			SELECT positive_centroid::text, negative_centroid::text, liked_count, disliked_count
			  FROM user_taste WHERE user_id = $1 FOR UPDATE`, in.UserID).Scan(&pos, &neg, &t.liked, &t.disliked); err != nil {
			return fmt.Errorf("locking taste of %s: %w", in.UserID, err)
		}
		if pos != nil {
			if t.pos, err = ParseVector(*pos); err != nil {
				return fmt.Errorf("positive centroid of %s: %w", in.UserID, err)
			}
		}
		if neg != nil {
			if t.neg, err = ParseVector(*neg); err != nil {
				return fmt.Errorf("negative centroid of %s: %w", in.UserID, err)
			}
		}
		wasLiked, wasDisliked := t.liked, t.disliked // the guard of the final UPDATE

		var ok bool
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
		var emb string
		err = tx.QueryRow(ctx, `SELECT embedding::text FROM clips WHERE id = $1`, in.ClipID).Scan(&emb)
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("reading clip %s: %w", in.ClipID, err)
		}
		v, err := ParseVector(emb)
		if err != nil {
			return fmt.Errorf("embedding of %s: %w", in.ClipID, err)
		}

		var prevAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT response, created_at FROM feedback_events
			 WHERE user_id = $1 AND clip_id = $2
			 ORDER BY created_at DESC, id DESC
			 LIMIT 1`, in.UserID, in.ClipID).Scan(&previous, &prevAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("reading current rating: %w", err)
		}
		prev := ""
		if previous != nil {
			prev = *previous
		}
		if prev == in.Response {
			return nil
		}
		t.move(prev, in.Response, v)

		// created_at is NOT the column default: now() is the transaction's START, and a request
		// that began first but waited on the lock would append a row that sorts before the one
		// it follows — the next reader would then take a stale prev and move the wrong vector.
		// clock_timestamp() is read under the lock; GREATEST keeps it strictly after prev even if
		// the wall clock steps back (GREATEST ignores a NULL prevAt).
		if _, err := tx.Exec(ctx, `
			INSERT INTO feedback_events (session_id, user_id, clip_id, response, replay_count, watched_full, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, GREATEST(clock_timestamp(), $7::timestamptz + interval '1 microsecond'))`,
			in.SessionID, in.UserID, in.ClipID, in.Response, in.ReplayCount, in.WatchedFull, prevAt); err != nil {
			return fmt.Errorf("appending feedback: %w", err)
		}
		// Never removed: a re-rated clip is still seen.
		if _, err := tx.Exec(ctx, `
			INSERT INTO seen_clips (user_id, clip_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			in.UserID, in.ClipID); err != nil {
			return fmt.Errorf("marking clip seen: %w", err)
		}
		// Guarded by the counts read under FOR UPDATE: 0 rows means the lock did not hold.
		// A nil centroid is written as NULL (taste_*_chk); it is never sent to a vector operator.
		tag, err = tx.Exec(ctx, `
			UPDATE user_taste
			   SET positive_centroid = $2::vector, negative_centroid = $3::vector,
			       liked_count = $4, disliked_count = $5, updated_at = now()
			 WHERE user_id = $1 AND liked_count = $6 AND disliked_count = $7`,
			in.UserID, literalOrNil(t.pos), literalOrNil(t.neg), t.liked, t.disliked, wasLiked, wasDisliked)
		if err != nil {
			return fmt.Errorf("updating taste of %s: %w", in.UserID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("updating taste of %s: %d rows, want 1", in.UserID, tag.RowsAffected())
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return changed, previous, nil
}
