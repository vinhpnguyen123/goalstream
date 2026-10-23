package recommend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/config"
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

// Recommender selects clips (plan §11). Its randomness is injected, so tests seed it.
type Recommender struct {
	Pool *pgxpool.Pool
	Cfg  config.RecoConfig
	mu   sync.Mutex // *rand.Rand is not safe for concurrent use
	rng  *rand.Rand
}

func NewRecommender(pool *pgxpool.Pool, cfg config.RecoConfig, rng *rand.Rand) *Recommender {
	return &Recommender{Pool: pool, Cfg: cfg, rng: rng}
}

// ponytail: no tag-based "because you liked header, long_range" (cut-line #2); add it when the
// generic reasons stop being enough to explain a pick.
const (
	reasonCold    = "getting to know you"
	reasonExploit = "similar to clips you loved"
	reasonAvoid   = "unlike clips you passed on" // exploit with dislikes only: §11 names no reason
	reasonExplore = "something different"
)

// Next picks the caller's next clip. Eligible = all clips − seen_clips(user) − exclude, where
// exclude is what this session served but the user did not rate (skipped, failed to load).
//
//	liked + disliked < COLD_START_THRESHOLD → cold: uniform among eligible, no centroid read
//	else with probability EXPLORE_RATE      → explore: uniform below the median similarity to
//	                                          pos (empty pool → exploit)
//	else                                    → exploit: top K by distance in SQL, re-ranked in Go
func (r *Recommender) Next(ctx context.Context, userID string, exclude []string) (Pick, error) {
	if exclude == nil {
		exclude = []string{} // a nil slice is SQL NULL, and `<> ALL(NULL)` matches no row at all
	}
	// Both draws up front: the lock is held for two numbers, never across a query.
	r.mu.Lock()
	coin, u := r.rng.Float64(), r.rng.Float64()
	r.mu.Unlock()

	var liked, disliked int
	err := r.Pool.QueryRow(ctx, `SELECT liked_count, disliked_count FROM user_taste WHERE user_id = $1`,
		userID).Scan(&liked, &disliked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) { // no row yet: never rated, 0/0
		return Pick{}, fmt.Errorf("reading taste counts: %w", err)
	}
	if liked+disliked < r.Cfg.ColdStartThreshold {
		return r.uniform(ctx, userID, exclude, u, nil, "cold", reasonCold)
	}

	var posText, negText *string
	if err := r.Pool.QueryRow(ctx, `
		SELECT positive_centroid::text, negative_centroid::text FROM user_taste WHERE user_id = $1`,
		userID).Scan(&posText, &negText); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Pick{}, fmt.Errorf("reading centroids: %w", err)
	}
	if posText == nil && negText == nil { // only reachable with COLD_START_THRESHOLD=0
		return r.uniform(ctx, userID, exclude, u, nil, "cold", reasonCold)
	}

	if coin < r.Cfg.ExploreRate {
		p, err := r.uniform(ctx, userID, exclude, u, posText, "explore", reasonExplore)
		if err != nil || !p.Exhausted {
			return p, err
		}
		// Empty pool (≤ 1 eligible clip, or every similarity tied): exploit instead.
	}

	// Exploit. The NULL branch is decided here, so a NULL vector never reaches `<=>`.
	var pos, neg []float32
	for _, c := range []struct {
		text *string
		out  *[]float32
	}{{posText, &pos}, {negText, &neg}} {
		if c.text != nil {
			if *c.out, err = ParseVector(*c.text); err != nil {
				return Pick{}, fmt.Errorf("centroid of %s: %w", userID, err)
			}
		}
	}
	reason, anchor, farthest := reasonExploit, posText, false
	if pos == nil {
		reason, anchor, farthest = reasonAvoid, negText, true
	}
	cands, err := r.topK(ctx, userID, exclude, *anchor, farthest)
	if err != nil {
		return Pick{}, err
	}
	if len(cands) == 0 {
		return Pick{Mode: "exploit", Exhausted: true}, nil
	}
	best := cands[rerank(cands, pos, neg, r.Cfg.NegPenalty)].clip
	return Pick{Clip: &best, Mode: "exploit", Reason: reason}, nil
}

// eligible is the WHERE of every selection query: $1 user, $2 exclude.
const eligible = `
	NOT EXISTS (SELECT 1 FROM seen_clips s WHERE s.user_id = $1 AND s.clip_id = c.id)
	AND c.id <> ALL($2::uuid[])`

// uniform picks the clip at floor(u·n) of the pool in id order: uniform for u in [0,1), and
// deterministic under a seeded RNG. pos nil = the pool is every eligible clip (cold); pos set =
// the eligible clips whose similarity to pos is below the median over eligible (explore).
// An empty pool is Exhausted.
func (r *Recommender) uniform(ctx context.Context, userID string, exclude []string, u float64,
	pos *string, mode, reason string) (Pick, error) {
	args := []any{userID, exclude, u}
	pool := `SELECT * FROM e`
	if pos != nil {
		args = append(args, *pos)
		pool = `SELECT * FROM e WHERE 1 - (embedding <=> $4::vector) <
		          (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY 1 - (embedding <=> $4::vector)) FROM e)`
	}
	var c db.ClipSummary
	// $3::float8: next to count(*) Postgres would infer bigint and truncate u to 0.
	err := r.Pool.QueryRow(ctx, `
		WITH e AS (SELECT c.id, c.title, c.match_minute, c.event_type, c.source_type, c.created_at, c.embedding
		             FROM clips c WHERE `+eligible+`),
		     pool AS (`+pool+`)
		SELECT id::text, title, match_minute, event_type, source_type, created_at
		  FROM pool ORDER BY id
		 OFFSET floor($3::float8 * (SELECT count(*) FROM pool))::bigint LIMIT 1`, args...).
		Scan(&c.ID, &c.Title, &c.MatchMinute, &c.EventType, &c.SourceType, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pick{Mode: mode, Exhausted: true}, nil
	} else if err != nil {
		return Pick{}, fmt.Errorf("picking %s clip: %w", mode, err)
	}
	return Pick{Clip: &c, Mode: mode, Reason: reason}, nil
}

type candidate struct {
	clip db.ClipSummary
	vec  []float32
}

// topK is exploit stage 1: the RECO_TOP_K eligible clips nearest to vec (farthest when
// farthest), exact scan. ponytail: no index below ~1000 clips (docs/SCHEMA.md graduation).
func (r *Recommender) topK(ctx context.Context, userID string, exclude []string, vec string, farthest bool) ([]candidate, error) {
	order := `embedding <=> $3::vector, c.id`
	if farthest {
		order = `embedding <=> $3::vector DESC, c.id`
	}
	rows, err := r.Pool.Query(ctx, `
		SELECT c.id::text, c.title, c.match_minute, c.event_type, c.source_type, c.created_at, c.embedding::text
		  FROM clips c WHERE `+eligible+`
		 ORDER BY `+order+`
		 LIMIT $4`, userID, exclude, vec, r.Cfg.TopK)
	if err != nil {
		return nil, fmt.Errorf("reading exploit candidates: %w", err)
	}
	var out []candidate
	var c candidate
	var emb string
	_, err = pgx.ForEachRow(rows, []any{&c.clip.ID, &c.clip.Title, &c.clip.MatchMinute, &c.clip.EventType,
		&c.clip.SourceType, &c.clip.CreatedAt, &emb}, func() error {
		v, err := ParseVector(emb)
		if err != nil {
			return fmt.Errorf("embedding of %s: %w", c.clip.ID, err)
		}
		out = append(out, candidate{c.clip, v})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading exploit candidates: %w", err)
	}
	return out, nil
}

// rerank is exploit stage 2 — a single ORDER BY cannot express distance to two vectors. It
// returns the index of the best candidate; ties keep stage 1's order.
//
//	pos set: cos(v,pos) − penalty·cos(v,neg)   (neg nil → no penalty)
//	pos nil: −cos(v,neg)
func rerank(cands []candidate, pos, neg []float32, penalty float64) int {
	best, bestScore := 0, math.Inf(-1)
	for i, c := range cands {
		var s float64
		if pos != nil {
			s = cosine(c.vec, pos)
			if neg != nil {
				s -= penalty * cosine(c.vec, neg)
			}
		} else {
			s = -cosine(c.vec, neg)
		}
		if s > bestScore {
			best, bestScore = i, s
		}
	}
	return best
}

// cosine is 0 against a zero vector instead of NaN: a centroid can cancel out to zero.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
