package recommend

import (
	"context"
	"fmt"
	"math/rand/v2"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

// Pick is GET /sessions/{id}/next. Nothing eligible is Exhausted with a nil Clip — not an
// error: the UI shows the summary.
type Pick struct {
	Clip      *db.ClipSummary `json:"clip"`
	Mode      string          `json:"mode"`
	Reason    string          `json:"reason"`
	Exhausted bool            `json:"exhausted"`
}

// Next picks the caller's next clip. Eligible = all clips − seen_clips(user) − exclude, where
// exclude is what this session served but the user did not rate (skipped, failed to load).
//
// M2 serves mode 'random': uniform among eligible. M4 adds cold / exploit / explore (plan §11)
// and an injected, seedable RNG.
func Next(ctx context.Context, pool *pgxpool.Pool, userID string, exclude []string) (Pick, error) {
	if exclude == nil {
		exclude = []string{} // a nil slice is SQL NULL, and `<> ALL(NULL)` matches no row at all
	}
	// ponytail: loads every eligible row and picks in Go — fine below ~1000 clips; M4 replaces
	// it with scans in SQL.
	rows, err := pool.Query(ctx, `
		SELECT c.id::text, c.title, c.match_minute, c.event_type, c.source_type, c.created_at
		  FROM clips c
		 WHERE NOT EXISTS (SELECT 1 FROM seen_clips s WHERE s.user_id = $1 AND s.clip_id = c.id)
		   AND c.id <> ALL($2::uuid[])`, userID, exclude)
	if err != nil {
		return Pick{}, fmt.Errorf("reading eligible clips: %w", err)
	}
	var eligible []db.ClipSummary
	var c db.ClipSummary
	_, err = pgx.ForEachRow(rows, []any{&c.ID, &c.Title, &c.MatchMinute, &c.EventType, &c.SourceType, &c.CreatedAt},
		func() error { eligible = append(eligible, c); return nil })
	if err != nil {
		return Pick{}, fmt.Errorf("reading eligible clips: %w", err)
	}
	if len(eligible) == 0 {
		return Pick{Mode: "random", Exhausted: true}, nil
	}
	pick := eligible[rand.IntN(len(eligible))]
	return Pick{Clip: &pick, Mode: "random", Reason: "random pick"}, nil
}
