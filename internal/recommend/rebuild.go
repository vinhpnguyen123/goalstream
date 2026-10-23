package recommend

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LockRebuild serializes everything that reads embedding inputs and writes embeddings
// (tag loads, rebuilds; centroid rebuilds from M4). Take it FIRST in the transaction, before
// any row is read or locked: a writer that locks clip rows and then waits for this lock
// deadlocks against a rebuild holding it. Transaction-scoped, re-entrant within a session.
func LockRebuild(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rebuild:embeddings'))`); err != nil {
		return fmt.Errorf("locking embedding rebuild: %w", err)
	}
	return nil
}

// lockRebuildShared is LockRebuild's reader side, taken FIRST by Rate. Rates run concurrently
// with each other but never with a rebuild, so (1) the vector a Rate subtracts is the one that
// was added — a rebuild changes vectors and centroids together — and (2) a rebuild never
// computes centroids from a feedback snapshot that misses a Rate committing under it.
func lockRebuildShared(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('rebuild:embeddings'))`); err != nil {
		return fmt.Errorf("locking embedding rebuild (shared): %w", err)
	}
	return nil
}

// ReembedAll recomputes every clip embedding from its columns + vocab, then every user's
// centroids from the new vectors (docs/SCHEMA.md § Rebuilds). Run it inside the transaction
// that changed the inputs, so a reader never sees new tags with an old vector, nor a centroid
// that still averages the old ones.
func ReembedAll(ctx context.Context, tx pgx.Tx) (int, error) {
	// Without the lock, a rebuild that read old tags could commit after a tag load and
	// overwrite its vectors with ones computed from the old tags.
	if err := LockRebuild(ctx, tx); err != nil {
		return 0, err
	}
	// LEFT JOINs + a NULL check instead of inner joins: a clip whose vocab row is missing must
	// abort the rebuild, not silently keep its stale vector.
	rows, err := tx.Query(ctx, `
		SELECT c.id::text, tv.slot, cv.slot, sv.slot, c.match_minute, c.is_home,
		       ARRAY(SELECT v.slot FROM vocab v WHERE v.kind = 'tag' AND v.value = ANY(c.tags)),
		       cardinality(c.tags), c.scoring_team IS NOT NULL
		  FROM clips c
		  JOIN matches m ON m.id = c.match_id
		  LEFT JOIN vocab tv ON tv.kind = 'team'        AND tv.value = c.scoring_team
		  LEFT JOIN vocab cv ON cv.kind = 'competition' AND cv.value = m.competition
		  LEFT JOIN vocab sv ON sv.kind = 'season'      AND sv.value = m.season
		 ORDER BY c.id`)
	if err != nil {
		return 0, fmt.Errorf("reading clips for reembed: %w", err)
	}
	type clip struct {
		id  string
		vec []float32
	}
	var clips []clip
	var (
		id                 string
		team, comp, season *int
		f                  Features
		tagSlots           []int32
		nTags              int
		hasTeam            bool
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &team, &comp, &season, &f.MatchMinute, &f.IsHome, &tagSlots, &nTags, &hasTeam}, func() error {
		if comp == nil || season == nil || (hasTeam && team == nil) || len(tagSlots) != nTags {
			return fmt.Errorf("clip %s references a value missing from vocab", id)
		}
		f.TeamSlot, f.CompSlot, f.SeasonSlot = team, *comp, *season
		f.TagSlots = f.TagSlots[:0]
		for _, s := range tagSlots {
			f.TagSlots = append(f.TagSlots, int(s))
		}
		v, err := Embed(f)
		if err != nil {
			return fmt.Errorf("clip %s: %w", id, err)
		}
		clips = append(clips, clip{id, v})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("reembed: %w", err)
	}
	for _, c := range clips {
		if _, err := tx.Exec(ctx, `UPDATE clips SET embedding = $2::vector WHERE id = $1`, c.id, Literal(c.vec)); err != nil {
			return 0, fmt.Errorf("updating embedding of %s: %w", c.id, err)
		}
	}
	if err := rebuildTaste(ctx, tx, nil); err != nil {
		return 0, err
	}
	return len(clips), nil
}

// rebuildTaste recomputes user_taste exactly from current ratings (the latest feedback_events
// row per clip): positive = mean of the 'loved' vectors, negative = mean of the 'not_for_me'
// ones. userID nil = every user. It also clears the float drift that online subtract-then-add
// accumulates. The caller holds what keeps vectors still: LockRebuild (ReembedAll), or the
// shared lock plus the user's new user_taste row (Rate).
func rebuildTaste(ctx context.Context, tx pgx.Tx, userID *string) error {
	// Reset first: a user whose directional ratings are all gone has no row in the upsert below.
	if _, err := tx.Exec(ctx, `
		UPDATE user_taste
		   SET positive_centroid = NULL, negative_centroid = NULL,
		       liked_count = 0, disliked_count = 0, updated_at = now()
		 WHERE $1::uuid IS NULL OR user_id = $1`, userID); err != nil {
		return fmt.Errorf("resetting taste: %w", err)
	}
	// AVG over no rows is NULL and count is 0 — exactly taste_*_chk. pgvector accumulates in
	// float8.
	if _, err := tx.Exec(ctx, `
		WITH cur AS (
		  SELECT DISTINCT ON (user_id, clip_id) user_id, clip_id, response
		    FROM feedback_events
		   WHERE $1::uuid IS NULL OR user_id = $1
		   ORDER BY user_id, clip_id, created_at DESC, id DESC)
		INSERT INTO user_taste (user_id, positive_centroid, negative_centroid, liked_count, disliked_count)
		SELECT cur.user_id,
		       AVG(c.embedding) FILTER (WHERE cur.response = 'loved'),
		       AVG(c.embedding) FILTER (WHERE cur.response = 'not_for_me'),
		       count(*) FILTER (WHERE cur.response = 'loved'),
		       count(*) FILTER (WHERE cur.response = 'not_for_me')
		  FROM cur JOIN clips c ON c.id = cur.clip_id
		 WHERE cur.response <> 'okay'
		 GROUP BY cur.user_id
		ON CONFLICT (user_id) DO UPDATE
		   SET positive_centroid = EXCLUDED.positive_centroid,
		       negative_centroid = EXCLUDED.negative_centroid,
		       liked_count = EXCLUDED.liked_count,
		       disliked_count = EXCLUDED.disliked_count,
		       updated_at = now()`, userID); err != nil {
		return fmt.Errorf("rebuilding taste: %w", err)
	}
	return nil
}
