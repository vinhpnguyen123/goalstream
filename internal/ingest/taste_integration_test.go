//go:build integration

package ingest

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/recommend"
)

// §12 #40 (rebuild part): a tag load changes clip vectors, and the same transaction rebuilds
// every centroid as the exact mean of the NEW vectors of current ratings — stale or drifted
// rows included — and empties a taste whose directional ratings are gone.
func TestTagLoadRebuildsCentroids(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	ids := seedClips(t, e, 4) // A B C D, identical vectors until the tags below
	a, b, c, d := ids[0], ids[1], ids[2], ids[3]

	newUser := func() string {
		var id string
		if err := e.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, '!') RETURNING id`,
			uuid.NewString()+"@x").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	u, v := newUser(), newUser()
	// U's current ratings: A loved, B loved, C not_for_me, D okay (loved, then re-rated).
	for i, r := range []struct{ clip, response string }{
		{a, "loved"}, {b, "loved"}, {c, "not_for_me"}, {d, "loved"}, {d, "okay"},
	} {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO feedback_events (user_id, clip_id, response, created_at)
			VALUES ($1, $2, $3, now() - make_interval(secs => $4))`, u, r.clip, r.response, 10-i); err != nil {
			t.Fatal(err)
		}
	}
	// Garbage in both rows: counts and vectors that match nothing (stale, or drifted).
	junk := make([]float32, recommend.Dims)
	junk[5] = 7
	for _, user := range []string{u, v} {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO user_taste (user_id, positive_centroid, negative_centroid, liked_count, disliked_count)
			VALUES ($1, $2::vector, $2::vector, 9, 9)`, user, recommend.Literal(junk)); err != nil {
			t.Fatal(err)
		}
	}

	sheet := writeSheet(t, a+",t,10,k,header", b+",t,10,k,volley", c+",t,10,k,penalty", d+",t,10,k,winner")
	if _, err := LoadTags(ctx, e.pool, sheet); err != nil {
		t.Fatal(err)
	}

	vec := func(query string, args ...any) []float32 {
		t.Helper()
		var s *string
		if err := e.pool.QueryRow(ctx, query, args...).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return nil
		}
		out, err := recommend.ParseVector(*s)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	emb := func(clip string) []float32 { return vec(`SELECT embedding::text FROM clips WHERE id = $1`, clip) }
	va, vb, vc := emb(a), emb(b), emb(c)
	if va[80] == 0 || vb[89] == 0 { // header = tag slot 0, volley = 9; the tag block starts at dim 80
		t.Fatal("the tag load did not change the vectors")
	}
	want := make([]float32, recommend.Dims)
	for i := range want {
		want[i] = (va[i] + vb[i]) / 2
	}
	near := func(what string, got, want []float32) {
		t.Helper()
		if (got == nil) != (want == nil) {
			t.Fatalf("%s: nil=%v, want nil=%v", what, got == nil, want == nil)
		}
		for i := range want {
			if math.Abs(float64(got[i]-want[i])) > 1e-6 {
				t.Fatalf("%s dim %d: %v, want %v", what, i, got[i], want[i])
			}
		}
	}
	near("U positive", vec(`SELECT positive_centroid::text FROM user_taste WHERE user_id = $1`, u), want)
	near("U negative", vec(`SELECT negative_centroid::text FROM user_taste WHERE user_id = $1`, u), vc)
	if n := e.count(t, `SELECT count(*) FROM user_taste WHERE user_id = $1 AND liked_count = 2 AND disliked_count = 1`, u); n != 1 {
		t.Fatal("U counts, want 2 loved / 1 not_for_me (D is okay now)")
	}
	if n := e.count(t, `SELECT count(*) FROM user_taste WHERE user_id = $1 AND liked_count = 0 AND disliked_count = 0
		AND positive_centroid IS NULL AND negative_centroid IS NULL`, v); n != 1 {
		t.Fatal("V has no ratings: want an empty taste")
	}
}
