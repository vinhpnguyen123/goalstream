//go:build integration

package recommend

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
)

// sqlLog records every statement the pool runs, so a test can assert what was NOT queried.
type sqlLog struct {
	mu  sync.Mutex
	sql []string
}

func (l *sqlLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	l.sql = append(l.sql, d.SQL)
	l.mu.Unlock()
	return ctx
}
func (l *sqlLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *sqlLog) reset() { l.mu.Lock(); l.sql = nil; l.mu.Unlock() }
func (l *sqlLog) count(substr string) (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sql {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

type recoEnv struct {
	pool  *pgxpool.Pool
	log   *sqlLog
	match string
}

func setupReco(t *testing.T) recoEnv {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	log := &sqlLog{}
	cfg.ConnConfig.Tracer = log
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE clips, matches, audit_log, users CASCADE`); err != nil {
		t.Fatal(err)
	}
	var match string
	if err := pool.QueryRow(ctx, `
		INSERT INTO matches (soccernet_path, home_team, away_team, competition, season)
		VALUES ('l/s/m', 'H', 'A', 'l', 's') RETURNING id`).Scan(&match); err != nil {
		t.Fatal(err)
	}
	return recoEnv{pool, log, match}
}

func (e recoEnv) reco(explore float64) *Recommender {
	return NewRecommender(e.pool, config.RecoConfig{
		ExploreRate: explore, ColdStartThreshold: 5, TopK: 50, NegPenalty: 0.5,
	}, rand.New(rand.NewPCG(7, 11)))
}

// cluster seeds n clips around axis (0 or 1), each with a small distinct offset.
func (e recoEnv) cluster(t *testing.T, axis, n int) []string {
	t.Helper()
	var ids []string
	for i := range n {
		v := make([]float32, Dims)
		v[axis] = 1
		v[10+axis*20+i] = 0.2
		v[2] = float32(i) / 100 // distinct similarities: a real catalogue has no exact ties
		id := uuid.NewString()
		if _, err := e.pool.Exec(context.Background(), `
			INSERT INTO clips (id, match_id, half, position_ms, event_type, title, match_minute, s3_prefix,
			                   width, height, source_bitrate, duration_ms, embedding)
			VALUES ($1,$2,1,$3,'Goal','t',1,'clips/x/',320,180,1,60000,$4::vector)`,
			id, e.match, axis*1000+i, Literal(v)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e recoEnv) user(t *testing.T) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (email, password_hash) VALUES ($1, '!') RETURNING id`,
		uuid.NewString()+"@x").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e recoEnv) rate(t *testing.T, user, response string, clips ...string) {
	t.Helper()
	for _, c := range clips {
		if _, _, err := Rate(context.Background(), e.pool, Rating{UserID: user, ClipID: c, Response: response}); err != nil {
			t.Fatal(err)
		}
	}
}

// serve asks for n picks in a row, excluding what was served, like a session that skips.
func serve(t *testing.T, r *Recommender, user string, n int) []Pick {
	t.Helper()
	var picks []Pick
	exclude := []string{}
	for range n {
		p, err := r.Next(context.Background(), user, exclude)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(p); err != nil { // a NaN anywhere would fail here
			t.Fatal(err)
		}
		picks = append(picks, p)
		if p.Clip != nil {
			exclude = append(exclude, p.Clip.ID)
		}
	}
	return picks
}

// §12 #17: opposite feedback → next-10 overlap ≤ 3; never a seen or excluded clip.
func TestNextDivergence(t *testing.T) {
	e := setupReco(t)
	x, y := e.cluster(t, 0, 15), e.cluster(t, 1, 15)
	u1, u2 := e.user(t), e.user(t)
	e.rate(t, u1, "loved", x[:5]...)
	e.rate(t, u1, "not_for_me", y[:5]...)
	e.rate(t, u2, "loved", y[:5]...)
	e.rate(t, u2, "not_for_me", x[:5]...)

	r := e.reco(0)
	got := map[string]map[string]bool{}
	for _, u := range []string{u1, u2} {
		seen := map[string]bool{}
		for _, id := range append(x[:5:5], y[:5]...) {
			seen[id] = true
		}
		got[u] = map[string]bool{}
		for i, p := range serve(t, r, u, 10) {
			if p.Clip == nil || p.Mode != "exploit" {
				t.Fatalf("user pick %d: %+v, want an exploit clip", i, p)
			}
			if seen[p.Clip.ID] || got[u][p.Clip.ID] {
				t.Fatalf("pick %d (%s) was seen or already served (excluded)", i, p.Clip.ID)
			}
			got[u][p.Clip.ID] = true
		}
	}
	overlap := 0
	for id := range got[u1] {
		if got[u2][id] {
			overlap++
		}
	}
	if overlap > 3 {
		t.Fatalf("next-10 overlap %d, want ≤ 3", overlap)
	}
	for _, id := range x[5:] {
		if !got[u1][id] {
			t.Fatalf("u1 loves cluster X but was not served all of its unseen clips")
		}
	}
}

// §12 #18: cold below the threshold with no centroid query; 'okay' stays cold; dislikes only
// is warm and serves the clip farthest from neg.
func TestNextColdStart(t *testing.T) {
	e := setupReco(t)
	x, y := e.cluster(t, 0, 8), e.cluster(t, 1, 8)
	r := e.reco(0)

	for name, setup := range map[string]func(u string){
		"never rated": func(string) {},
		"4 directional": func(u string) {
			e.rate(t, u, "loved", x[:2]...)
			e.rate(t, u, "not_for_me", y[:2]...)
		},
		"5 okay": func(u string) { e.rate(t, u, "okay", x[:5]...) },
	} {
		u := e.user(t)
		setup(u)
		e.log.reset()
		p := serve(t, r, u, 1)[0]
		if p.Mode != "cold" || p.Reason != reasonCold || p.Clip == nil {
			t.Fatalf("%s: %+v, want cold", name, p)
		}
		if n := e.log.count("centroid"); n != 0 {
			t.Fatalf("%s: %d centroid queries on a cold pick, want 0", name, n)
		}
	}

	u := e.user(t)
	e.rate(t, u, "not_for_me", x[:5]...)
	// TopK below the eligible count, so stage 1's direction matters, not just stage 2's score.
	narrow := e.reco(0)
	narrow.Cfg.TopK = 3
	p := serve(t, narrow, u, 1)[0]
	if p.Mode != "exploit" || p.Reason != reasonAvoid || p.Clip == nil {
		t.Fatalf("5 dislikes: %+v, want warm exploit away from neg", p)
	}
	if !contains(y, p.Clip.ID) {
		t.Fatalf("5 dislikes of X served %s, want a clip from Y (farthest from neg)", p.Clip.ID)
	}
}

// §12 #19: everything seen → exhausted; an empty explore pool falls back to exploit.
func TestNextExhaustionAndEmptyExplorePool(t *testing.T) {
	e := setupReco(t)
	x := e.cluster(t, 0, 6)

	u := e.user(t)
	e.rate(t, u, "loved", x[:5]...)
	// One eligible clip left: the below-median pool is empty, so explore cannot serve it.
	p := serve(t, e.reco(1), u, 1)[0]
	if p.Mode != "exploit" || p.Clip == nil || p.Clip.ID != x[5] {
		t.Fatalf("explore with one eligible: %+v, want exploit of %s", p, x[5])
	}

	e.rate(t, u, "okay", x[5])
	for _, rate := range []float64{0, 1} {
		p := serve(t, e.reco(rate), u, 1)[0]
		if p.Clip != nil || !p.Exhausted {
			t.Fatalf("all seen (explore %v): %+v, want exhausted", rate, p)
		}
	}
	cold := e.user(t)
	if p := serve(t, e.reco(0), cold, 7)[6]; p.Clip != nil || !p.Exhausted || p.Mode != "cold" {
		t.Fatalf("cold user, all excluded: %+v, want exhausted", p)
	}
}

// Explore serves only clips below the median similarity to pos.
func TestNextExploreBelowMedian(t *testing.T) {
	e := setupReco(t)
	x, y := e.cluster(t, 0, 10), e.cluster(t, 1, 10)
	u := e.user(t)
	e.rate(t, u, "loved", x[:5]...)
	for i, p := range serve(t, e.reco(1), u, 5) {
		if p.Mode != "explore" || p.Reason != reasonExplore || !contains(y, p.Clip.ID) {
			t.Fatalf("explore pick %d: %+v, want a Y clip (X is above the median)", i, p)
		}
	}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
