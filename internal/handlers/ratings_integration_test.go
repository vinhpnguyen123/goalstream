//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/recommend"
)

func (e apiEnv) send(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:1234"
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %s: not JSON: %q", method, path, rec.Body)
	}
	return rec, resp
}

func (e apiEnv) putRating(t *testing.T, clip, response string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return e.send(t, "PUT", "/api/v1/ratings/"+clip, fmt.Sprintf(`{"response":%q}`, response))
}

// seedVecClip is seedClip with a vector of its own, so a mean over several clips is checkable.
func (e apiEnv) seedVecClip(t *testing.T, k int) (string, []float32) {
	t.Helper()
	id := e.seedClip(t, fmt.Sprintf("clip %d", k), time.Now(), []byte("x"))
	v := make([]float32, recommend.Dims)
	v[k%recommend.Dims] = 1
	v[(k+7)%recommend.Dims] = float32(k) / 10
	if _, err := e.s.Pool.Exec(context.Background(), `UPDATE clips SET embedding = $2::vector WHERE id = $1`,
		id, recommend.Literal(v)); err != nil {
		t.Fatal(err)
	}
	return id, v
}

type tasteRow struct {
	pos, neg        []float32
	liked, disliked int
}

func (e apiEnv) taste(t *testing.T) tasteRow {
	t.Helper()
	var pos, neg *string
	var r tasteRow
	if err := e.s.Pool.QueryRow(context.Background(), `
		SELECT positive_centroid::text, negative_centroid::text, liked_count, disliked_count
		  FROM user_taste WHERE user_id = $1`, e.s.DevUserID).Scan(&pos, &neg, &r.liked, &r.disliked); err != nil {
		t.Fatal(err)
	}
	parse := func(s *string) []float32 {
		if s == nil {
			return nil
		}
		v, err := recommend.ParseVector(*s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	r.pos, r.neg = parse(pos), parse(neg)
	return r
}

func mean(vs ...[]float32) []float32 {
	out := make([]float32, recommend.Dims)
	for _, v := range vs {
		for i := range v {
			out[i] += v[i] / float32(len(vs))
		}
	}
	return out
}

func sameVec(t *testing.T, what string, got, want []float32) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: got nil=%v, want nil=%v", what, got == nil, want == nil)
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-5 {
			t.Fatalf("%s dim %d: got %v, want %v", what, i, got[i], want[i])
		}
	}
}

// §12 #16: 20 concurrent 'loved' on 20 clips → liked_count 20 and the exact mean.
func TestCentroidConcurrentLoves(t *testing.T) {
	e := setupAPI(t)
	sess := e.newSession(t)
	var ids []string
	var vecs [][]float32
	for k := range 20 {
		id, v := e.seedVecClip(t, k)
		ids, vecs = append(ids, id), append(vecs, v)
	}
	var wg sync.WaitGroup
	codes := make([]int, len(ids))
	for i, id := range ids {
		wg.Go(func() {
			rec, _ := e.postJSON(t, "/api/v1/feedback", feedbackBody(sess, id, "loved"))
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	got := e.taste(t)
	if got.liked != 20 || got.disliked != 0 || got.neg != nil {
		t.Fatalf("counts %d/%d neg nil=%v, want 20/0 nil", got.liked, got.disliked, got.neg == nil)
	}
	sameVec(t, "positive centroid", got.pos, mean(vecs...))
}

// §12 #40 (online): the vector moves between centroids, and removing the last one is NULL.
func TestReRateMovesVector(t *testing.T) {
	e := setupAPI(t)
	a, va := e.seedVecClip(t, 1)
	b, vb := e.seedVecClip(t, 2)

	e.putRating(t, a, "loved")
	if got := e.taste(t); got.liked != 1 || got.disliked != 0 || got.neg != nil {
		t.Fatalf("after loved: %d/%d", got.liked, got.disliked)
	}
	_, body := e.putRating(t, a, "not_for_me")
	if prev := data(body)["previous"]; prev != "loved" {
		t.Fatalf("previous = %v, want loved", prev)
	}
	got := e.taste(t)
	if got.liked != 0 || got.disliked != 1 || got.pos != nil {
		t.Fatalf("loved→not_for_me: counts %d/%d pos nil=%v, want 0/1 nil", got.liked, got.disliked, got.pos == nil)
	}
	sameVec(t, "negative centroid", got.neg, va)

	// loved→okay with n=1: remove only, positive back to NULL.
	e.putRating(t, b, "loved")
	sameVec(t, "positive centroid", e.taste(t).pos, vb)
	e.putRating(t, b, "okay")
	if got := e.taste(t); got.liked != 0 || got.pos != nil || got.disliked != 1 {
		t.Fatalf("loved→okay: counts %d/%d pos nil=%v", got.liked, got.disliked, got.pos == nil)
	}

	// okay→loved: add only; a stays in the negative centroid.
	e.putRating(t, b, "loved")
	got = e.taste(t)
	sameVec(t, "positive centroid", got.pos, vb)
	sameVec(t, "negative centroid", got.neg, va)
}

// §12 #40: the same response twice is {changed:false} and no row; the centroid is untouched.
func TestReRateSameResponseTwice(t *testing.T) {
	e := setupAPI(t)
	a, va := e.seedVecClip(t, 3)
	e.putRating(t, a, "loved")
	rec, body := e.putRating(t, a, "loved")
	if rec.Code != 200 || data(body)["changed"] != false {
		t.Fatalf("%d %v, want 200 changed:false", rec.Code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM feedback_events WHERE clip_id = $1`, a); n != 1 {
		t.Fatalf("%d feedback rows, want 1", n)
	}
	got := e.taste(t)
	if got.liked != 1 {
		t.Fatalf("liked_count %d, want 1", got.liked)
	}
	sameVec(t, "positive centroid", got.pos, va)
}

// §12 #40: 20 concurrent alternating re-rates of one clip leave counts and centroids that match
// the final current rating — no lost update, no vector removed twice.
func TestConcurrentAlternatingReRates(t *testing.T) {
	e := setupAPI(t)
	a, va := e.seedVecClip(t, 4)
	var wg sync.WaitGroup
	for i := range 20 {
		resp := "loved"
		if i%2 == 1 {
			resp = "not_for_me"
		}
		wg.Go(func() {
			if rec, _ := e.putRating(t, a, resp); rec.Code != 200 {
				t.Errorf("PUT %s: %d", resp, rec.Code)
			}
		})
	}
	wg.Wait()
	var current string
	if err := e.s.Pool.QueryRow(context.Background(), `
		SELECT response FROM feedback_events WHERE clip_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT 1`, a).Scan(&current); err != nil {
		t.Fatal(err)
	}
	got := e.taste(t)
	switch current {
	case "loved":
		if got.liked != 1 || got.disliked != 0 || got.neg != nil {
			t.Fatalf("current loved: counts %d/%d", got.liked, got.disliked)
		}
		sameVec(t, "positive centroid", got.pos, va)
	case "not_for_me":
		if got.liked != 0 || got.disliked != 1 || got.pos != nil {
			t.Fatalf("current not_for_me: counts %d/%d", got.liked, got.disliked)
		}
		sameVec(t, "negative centroid", got.neg, va)
	}
}

// PUT /ratings rates outside a session: allowed on a never-rated clip, NULL session and
// watched_full; malformed or unknown id 404, bad response 400.
func TestPutRating(t *testing.T) {
	e := setupAPI(t)
	a, _ := e.seedVecClip(t, 5)
	rec, body := e.putRating(t, a, "okay")
	if rec.Code != 200 || data(body)["changed"] != true || data(body)["previous"] != nil {
		t.Fatalf("%d %v, want changed:true previous:null", rec.Code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM feedback_events
		WHERE clip_id = $1 AND session_id IS NULL AND watched_full IS NULL AND replay_count = 0`, a); n != 1 {
		t.Fatalf("%d out-of-session rows, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM seen_clips WHERE clip_id = $1`, a); n != 1 {
		t.Fatal("PUT /ratings must mark the clip seen")
	}
	for name, c := range map[string]struct {
		clip, body string
		code       int
		typ        string
	}{
		"malformed id":  {"not-a-uuid", `{"response":"loved"}`, 404, "not_found"},
		"unknown clip":  {uuid.NewString(), `{"response":"loved"}`, 404, "not_found"},
		"bad response":  {a, `{"response":"meh"}`, 400, "validation_error"},
		"no response":   {a, `{}`, 400, "validation_error"},
		"unknown field": {a, `{"response":"loved","session_id":"x"}`, 400, "validation_error"},
	} {
		rec, body := e.send(t, "PUT", "/api/v1/ratings/"+c.clip, c.body)
		if rec.Code != c.code || errType(body) != c.typ {
			t.Errorf("%s: %d %s, want %d %s", name, rec.Code, errType(body), c.code, c.typ)
		}
	}
}

// GET /ratings: current ratings only, newest change first, keyset-paged, the caller's own.
func TestListRatings(t *testing.T) {
	e := setupAPI(t)
	var ids []string
	for k := range 3 {
		id, _ := e.seedVecClip(t, 10+k)
		ids = append(ids, id)
		e.putRating(t, id, "okay")
		time.Sleep(2 * time.Millisecond) // distinct rated_at, so the order is the rating order
	}
	e.putRating(t, ids[0], "loved") // a re-rate: ids[0] is now the newest change

	// Another user's rating of ids[1] is invisible.
	if _, err := e.s.Pool.Exec(context.Background(), `
		WITH u AS (INSERT INTO users (email, password_hash) VALUES ('other@localhost', '!') RETURNING id)
		INSERT INTO feedback_events (user_id, clip_id, response) SELECT id, $1, 'not_for_me' FROM u`, ids[1]); err != nil {
		t.Fatal(err)
	}

	_, body := e.get(t, "/api/v1/ratings?limit=2", true)
	page := data(body)
	items := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("%d items, want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["clip_id"] != ids[0] || first["response"] != "loved" || first["title"] != "clip 10" {
		t.Fatalf("first = %v, want ids[0] loved", first)
	}
	if items[1].(map[string]any)["clip_id"] != ids[2] {
		t.Fatalf("second = %v, want ids[2]", items[1])
	}
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("want a next_cursor")
	}
	_, body = e.get(t, "/api/v1/ratings?limit=2&cursor="+cursor, true)
	page = data(body)
	items = page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["clip_id"] != ids[1] || items[0].(map[string]any)["response"] != "okay" {
		t.Fatalf("page 2 = %v, want only ids[1] okay (mine, not the other user's)", items)
	}
	if page["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v, want null", page["next_cursor"])
	}

	for _, q := range []string{"cursor=garbage", "limit=0", "limit=101"} {
		if rec, body := e.get(t, "/api/v1/ratings?"+q, true); rec.Code != 400 || errType(body) != "validation_error" {
			t.Errorf("%s: %d %s, want 400", q, rec.Code, errType(body))
		}
	}
}

// A user rated before user_taste existed (M2): the first Rate builds the row from that history
// instead of starting empty — re-rating an old 'loved' used to drive liked_count to −1 (500).
func TestRateInitializesTasteFromPriorFeedback(t *testing.T) {
	e := setupAPI(t)
	a, _ := e.seedVecClip(t, 1)
	b, vb := e.seedVecClip(t, 2)
	c, vc := e.seedVecClip(t, 3)
	for i, r := range []struct{ clip, response string }{{a, "loved"}, {b, "not_for_me"}} {
		if _, err := e.s.Pool.Exec(context.Background(), `
			INSERT INTO feedback_events (user_id, clip_id, response, created_at)
			VALUES ($1, $2, $3, now() - make_interval(secs => $4))`, e.s.DevUserID, r.clip, r.response, 10-i); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM user_taste`); n != 0 {
		t.Fatal("precondition: no user_taste row")
	}

	if rec, body := e.putRating(t, a, "okay"); rec.Code != 200 || data(body)["previous"] != "loved" {
		t.Fatalf("re-rate of a pre-taste loved: %d %v", rec.Code, body)
	}
	got := e.taste(t)
	if got.liked != 0 || got.disliked != 1 || got.pos != nil {
		t.Fatalf("after loved→okay: counts %d/%d pos nil=%v, want 0/1 nil", got.liked, got.disliked, got.pos == nil)
	}
	sameVec(t, "negative centroid", got.neg, vb)

	e.putRating(t, c, "loved")
	got = e.taste(t)
	if got.liked != 1 || got.disliked != 1 {
		t.Fatalf("counts %d/%d, want 1/1", got.liked, got.disliked)
	}
	sameVec(t, "positive centroid", got.pos, vc)
}
