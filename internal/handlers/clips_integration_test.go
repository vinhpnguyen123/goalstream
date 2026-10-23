//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/recommend"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/storage/storagetest"
)

type apiEnv struct {
	s      *Server
	h      http.Handler
	cookie *http.Cookie
}

// setupAPI is the real stack: test DB (truncated), test bucket (emptied), live Redis,
// AUTH_MODE=dev behind an enabled gate.
func setupAPI(t *testing.T) apiEnv {
	t.Helper()
	ctx := context.Background()
	cfg := storagetest.Config(t)
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE clips, matches, audit_log, users CASCADE`); err != nil {
		t.Fatal(err)
	}
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { rdb.Close() })
	// Every test request comes from one TEST-NET address; without a fresh window, repeated
	// runs (-count=3) hit the default 300/min limit and fail with 429.
	keys, err := rdb.Keys(ctx, "rl:*:203.0.113.9:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) > 0 {
		if err := rdb.Del(ctx, keys...).Err(); err != nil {
			t.Fatal(err)
		}
	}
	dev, err := auth.UpsertDevUser(ctx, pool, "dev@localhost")
	if err != nil {
		t.Fatal(err)
	}
	cfg.API = gateConfig()
	cfg.API.AuthMode = "dev"
	cfg.Reco = config.RecoConfig{ExploreRate: 0.3, ColdStartThreshold: 5, TopK: 50, NegPenalty: 0.5}
	s := &Server{Pool: pool, Redis: rdb, Store: storage.New(cfg.S3), Cfg: cfg, DevUserID: dev,
		Reco: recommend.NewRecommender(pool, cfg.Reco, rand.New(rand.NewPCG(1, 2)))}
	return apiEnv{s, s.Routes(), &http.Cookie{
		Name: auth.InviteCookie, Value: auth.SignInvite(testSecret, "me", time.Now().Add(time.Hour)),
	}}
}

// seedClip uploads body as the clip's original.mp4 and inserts its row at createdAt.
func (e apiEnv) seedClip(t *testing.T, title string, createdAt time.Time, body []byte) string {
	t.Helper()
	ctx := context.Background()
	var matchID string
	if err := e.s.Pool.QueryRow(ctx, `
		INSERT INTO matches (soccernet_path, home_team, away_team, competition, season)
		VALUES ('l/s/m', 'H', 'A', 'l', 's')
		ON CONFLICT (soccernet_path) DO UPDATE SET home_team = EXCLUDED.home_team RETURNING id`).Scan(&matchID); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	f := filepath.Join(t.TempDir(), "original.mp4")
	if err := os.WriteFile(f, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Store.PutFile(ctx, "clips/"+id+"/original.mp4", f, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	vec, _ := recommend.Embed(recommend.Features{CompSlot: 0, SeasonSlot: 0})
	if _, err := e.s.Pool.Exec(ctx, `
		INSERT INTO clips (id, match_id, half, position_ms, event_type, title, match_minute, s3_prefix,
		                   width, height, source_bitrate, duration_ms, embedding, created_at)
		VALUES ($1,$2,1,$3,'Goal',$4,1,$5,320,180,1,60000,$6::vector,$7)`,
		id, matchID, rand.IntN(1_000_000_000), title, "clips/"+id+"/", recommend.Literal(vec), createdAt); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e apiEnv) get(t *testing.T, path string, withCookie bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	if withCookie {
		req.AddCookie(e.cookie)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: not JSON: %q", path, rec.Body)
	}
	return rec, body
}

func errType(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["type"].(string)
	return s
}

// §12 #11 at the router: the gate runs before auth, and its exemptions are exactly two.
func TestGateGuardsUserRoutes(t *testing.T) {
	e := setupAPI(t)
	id := e.seedClip(t, "c", time.Now(), []byte("x"))
	for _, p := range []string{"/api/v1/clips", "/api/v1/clips/" + id + "/play"} {
		// dev auth would admit anyone — the gate must still answer first.
		if rec, body := e.get(t, p, false); rec.Code != 401 || errType(body) != "gate_required" {
			t.Errorf("%s without cookie: %d %v", p, rec.Code, body)
		}
		if rec, _ := e.get(t, p, true); rec.Code != 200 {
			t.Errorf("%s with cookie: %d", p, rec.Code)
		}
	}
	if rec, _ := e.get(t, "/api/v1/healthz", false); rec.Code != 200 {
		t.Errorf("healthz must be exempt: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/gate", nil))
	if rec.Code != 400 { // reached the handler: an empty body is a validation error, not gate_required
		t.Errorf("POST /gate must be reachable without a cookie (got %d, want 400 for an empty body)", rec.Code)
	}

	// AUTH_MODE=closed: past the gate, every user route is 401 unauthorized.
	e.s.Cfg.API.AuthMode = "closed"
	closed := apiEnv{e.s, e.s.Routes(), e.cookie}
	if rec, body := closed.get(t, "/api/v1/clips", true); rec.Code != 401 || errType(body) != "unauthorized" {
		t.Errorf("closed mode: %d %v", rec.Code, body)
	}
}

// §12 #8 + #9: the API hands out a small JSON body; the bytes come from storage, with Range.
func TestPlayPresignsPublicURL(t *testing.T) {
	e := setupAPI(t)
	body := make([]byte, 5000)
	for i := range body {
		body[i] = byte(i)
	}
	id := e.seedClip(t, "c", time.Now(), body)

	rec, resp := e.get(t, "/api/v1/clips/"+id+"/play", true)
	if rec.Code != 200 {
		t.Fatalf("play: %d %v", rec.Code, resp)
	}
	if rec.Body.Len() >= 1024 {
		t.Fatalf("API response is %d bytes: media must not flow through the API", rec.Body.Len())
	}
	data := resp["data"].(map[string]any)
	if data["type"] != "mp4" {
		t.Fatalf("type %v", data["type"])
	}
	exp, err := time.Parse(time.RFC3339Nano, data["expires_at"].(string))
	if err != nil || exp.Before(time.Now()) || exp.After(time.Now().Add(e.s.Cfg.S3.PresignTTL+time.Minute)) {
		t.Fatalf("expires_at %v", data["expires_at"])
	}
	signed, _ := url.Parse(data["url"].(string))
	public, _ := url.Parse(e.s.Cfg.S3.PublicEndpoint)
	if signed.Host != public.Host {
		t.Fatalf("URL host %q, want the public endpoint %q", signed.Host, public.Host)
	}

	res, err := http.Get(signed.String())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || len(got) != len(body) || res.ContentLength != int64(len(body)) {
		t.Fatalf("GET presigned: %d, %d bytes (Content-Length %d), want %d", res.StatusCode, len(got), res.ContentLength, len(body))
	}

	req, _ := http.NewRequest("GET", signed.String(), nil)
	req.Header.Set("Range", "bytes=100-199")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || res.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(body)) ||
		len(part) != 100 || part[0] != body[100] {
		t.Fatalf("Range: %d %q, %d bytes", res.StatusCode, res.Header.Get("Content-Range"), len(part))
	}
}

func TestPlayNotFoundAndBadFormat(t *testing.T) {
	e := setupAPI(t)
	id := e.seedClip(t, "c", time.Now(), []byte("x"))
	for _, tc := range []struct {
		path, want string
		code       int
	}{
		{"/api/v1/clips/" + uuid.NewString() + "/play", "not_found", 404},
		{"/api/v1/clips/not-a-uuid/play", "not_found", 404},
		{"/api/v1/clips/" + id + "/play?format=hls", "validation_error", 400},
		{"/api/v1/clips/" + id + "/play?format=mp4", "", 200},
	} {
		rec, body := e.get(t, tc.path, true)
		if rec.Code != tc.code || errType(body) != tc.want {
			t.Errorf("%s: %d %v", tc.path, rec.Code, body)
		}
	}
}

func TestListClipsPaginates(t *testing.T) {
	e := setupAPI(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	var want []string // newest first
	for i := range 5 {
		want = append([]string{e.seedClip(t, fmt.Sprint(i), base.Add(time.Duration(i)*time.Second), []byte("x"))}, want...)
	}
	// Two clips at the same instant: the id breaks the tie, so neither is skipped or repeated.
	tie := base.Add(10 * time.Second)
	a, b := e.seedClip(t, "a", tie, []byte("x")), e.seedClip(t, "b", tie, []byte("x"))
	if a < b {
		a, b = b, a
	}
	want = append([]string{a, b}, want...)

	var got []string
	path := "/api/v1/clips?limit=2"
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination never ended")
		}
		rec, body := e.get(t, path, true)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %v", path, rec.Code, body)
		}
		data := body["data"].(map[string]any)
		for _, it := range data["items"].([]any) {
			got = append(got, it.(map[string]any)["id"].(string))
		}
		next, _ := data["next_cursor"].(string)
		if next == "" {
			break
		}
		path = "/api/v1/clips?limit=2&cursor=" + next
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pages gave\n%v\nwant\n%v", got, want)
	}

	for _, bad := range []string{"limit=0", "limit=101", "limit=abc", "cursor=!!!", "cursor=bm9wZQ"} {
		if rec, body := e.get(t, "/api/v1/clips?"+bad, true); rec.Code != 400 || errType(body) != "validation_error" {
			t.Errorf("%s: %d %v", bad, rec.Code, body)
		}
	}
	rec, body := e.get(t, "/api/v1/clips", true)
	if items := body["data"].(map[string]any)["items"].([]any); rec.Code != 200 || len(items) != 7 {
		t.Fatalf("default page: %d, %d items", rec.Code, len(items))
	}
}

// §12 #12 (second half, at the router): Redis down → /gate 503 (closed), /clips 200 (open).
func TestRedisDownSplitsByRule(t *testing.T) {
	e := setupAPI(t)
	e.seedClip(t, "c", time.Now(), []byte("x"))
	e.s.Redis = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	down := apiEnv{e.s, e.s.Routes(), e.cookie}

	if rec, _ := down.get(t, "/api/v1/clips", true); rec.Code != 200 {
		t.Errorf("/clips with Redis down: %d, want 200", rec.Code)
	}
	rec := httptest.NewRecorder()
	down.h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/gate", nil))
	if rec.Code != 503 {
		t.Errorf("/gate with Redis down: %d, want 503", rec.Code)
	}
}
