//go:build integration

package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/transcode"
)

// version records a committed hls version for the clip: what a fenced commit leaves behind
// (renditions rows), without running ffmpeg.
func (e apiEnv) version(t *testing.T, clipID string) string {
	t.Helper()
	v := uuid.NewString()
	if _, err := e.s.Pool.Exec(context.Background(), `
		INSERT INTO renditions (clip_id, hls_version, height, width, video_bitrate, max_rate, audio_bitrate, bandwidth, segment_count)
		VALUES ($1, $2, 360, 640, 600000, 642000, 96000, 774900, 15)`, clipID, v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (e apiEnv) setDelivery(t *testing.T, clipID, body string) (int, map[string]any) {
	t.Helper()
	rec, resp := e.postJSON(t, "/api/v1/admin/clips/"+clipID+"/delivery", body)
	return rec.Code, resp
}

// playKey is the object key /play presigned, and its type.
func (e apiEnv) playKey(t *testing.T, clipID, query string) (typ, key string) {
	t.Helper()
	rec, body := e.get(t, "/api/v1/clips/"+clipID+"/play"+query, true)
	if rec.Code != 200 {
		t.Fatalf("play: %d %v", rec.Code, body)
	}
	d := data(body)
	u, err := url.Parse(d["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return d["type"].(string), strings.TrimPrefix(u.Path, "/"+e.s.Cfg.S3.Bucket+"/")
}

func (e apiEnv) deliveryState(t *testing.T, clipID string) string {
	t.Helper()
	var st, ver string
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT source_type, coalesce(hls_version::text, '') FROM clips WHERE id = $1`, clipID).Scan(&st, &ver); err != nil {
		t.Fatal(err)
	}
	return st + "/" + ver
}

func TestCreateJob(t *testing.T) {
	e := setupAPI(t)
	clip := e.seedClip(t, "c", time.Now(), []byte("x"))

	rec, body := e.post(t, "/api/v1/admin/clips/"+clip+"/transcode")
	if rec.Code != 202 {
		t.Fatalf("transcode: %d %v", rec.Code, body)
	}
	job := data(body)["job_id"].(string)
	// Postgres first, then the push: the queue holds exactly this job.
	if q, err := e.s.Redis.LRange(context.Background(), e.s.Queue.NS+":queue", 0, -1).Result(); err != nil || len(q) != 1 || q[0] != job {
		t.Fatalf("queue = %v (%v), want [%s]", q, err, job)
	}

	rec, body = e.post(t, "/api/v1/admin/clips/"+clip+"/transcode")
	errObj, _ := body["error"].(map[string]any)
	if rec.Code != 409 || errType(body) != "job_active" || errObj["job_id"] != job {
		t.Fatalf("second transcode: %d %v, want 409 job_active naming %s", rec.Code, body, job)
	}
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		if rec, body := e.post(t, "/api/v1/admin/clips/"+id+"/transcode"); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("transcode %s: %d %v", id, rec.Code, body)
		}
	}
	var audits int
	e.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'job_created' AND entity_id = $1`, job).Scan(&audits)
	if audits != 1 {
		t.Fatalf("job_created audited %d times", audits)
	}

	// A finished (failed) job does not block a new one: retrying a clip is a NEW job.
	if _, err := e.s.Pool.Exec(context.Background(), `UPDATE transcode_jobs SET status = 'failed' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	if rec, body := e.post(t, "/api/v1/admin/clips/"+clip+"/transcode"); rec.Code != 202 {
		t.Fatalf("transcode after a failed job: %d %v", rec.Code, body)
	}
}

// §12 #35: two concurrent triggers for one clip → one 202, one 409, one job row.
func TestCreateJobConcurrent(t *testing.T) {
	e := setupAPI(t)
	for i := 0; i < 10; i++ {
		clip := e.seedClip(t, "c", time.Now(), []byte("x"))
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("POST", "/api/v1/admin/clips/"+clip+"/transcode", nil)
				req.RemoteAddr = "203.0.113.9:1234"
				req.AddCookie(e.cookie)
				rec := httptest.NewRecorder()
				e.h.ServeHTTP(rec, req)
				codes <- rec.Code
			}()
		}
		wg.Wait()
		close(codes)
		got := map[int]int{}
		for c := range codes {
			got[c]++
		}
		if got[202] != 1 || got[409] != 1 {
			t.Fatalf("round %d: codes %v, want one 202 and one 409", i, got)
		}
		var jobs int
		e.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM transcode_jobs WHERE clip_id = $1`, clip).Scan(&jobs)
		if jobs != 1 {
			t.Fatalf("round %d: %d job rows", i, jobs)
		}
	}
}

// Redis down: the push fails, the job is still accepted — the row is the job.
func TestCreateJobRedisDown(t *testing.T) {
	e := setupAPI(t)
	clip := e.seedClip(t, "c", time.Now(), []byte("x"))
	e.s.Queue.R = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	if rec, body := e.post(t, "/api/v1/admin/clips/"+clip+"/transcode"); rec.Code != 202 {
		t.Fatalf("transcode with Redis down: %d %v", rec.Code, body)
	}
	var st string
	e.s.Pool.QueryRow(context.Background(), `SELECT status FROM transcode_jobs WHERE clip_id = $1`, clip).Scan(&st)
	if st != "queued" {
		t.Fatalf("job status %q, want queued", st)
	}
}

// §12 #25 (+ the /play side of #26 and the ?format=mp4 fallback).
func TestRollback(t *testing.T) {
	ctx := context.Background()
	e := setupAPI(t)
	clip := e.seedClip(t, "c", time.Now(), []byte("x"))
	v1, v2 := e.version(t, clip), e.version(t, clip)
	if _, err := e.s.Pool.Exec(ctx, `UPDATE clips SET source_type = 'hls', hls_version = $2 WHERE id = $1`, clip, v2); err != nil {
		t.Fatal(err)
	}
	mp4Clip := e.seedClip(t, "plain", time.Now(), []byte("x"))

	// Mixed catalogue: one hls clip and one mp4 clip, each served its own way.
	if typ, key := e.playKey(t, clip, ""); typ != "hls" || key != "clips/"+clip+"/hls/"+v2+"/master.m3u8" {
		t.Fatalf("hls clip plays %s %s", typ, key)
	}
	if typ, key := e.playKey(t, mp4Clip, ""); typ != "mp4" || key != "clips/"+mp4Clip+"/original.mp4" {
		t.Fatalf("mp4 clip plays %s %s", typ, key)
	}
	// The player's fallback reaches the original whatever source_type says.
	if typ, key := e.playKey(t, clip, "?format=mp4"); typ != "mp4" || key != "clips/"+clip+"/original.mp4" {
		t.Fatalf("?format=mp4 plays %s %s", typ, key)
	}

	steps := []struct {
		name, body string
		code       int
		changed    bool
		state      string
	}{
		{"to mp4", `{"source_type":"mp4"}`, 200, true, "mp4/" + v2}, // the version is kept
		{"to mp4 again", `{"source_type":"mp4"}`, 200, false, "mp4/" + v2},
		{"re-promote the version just left", `{"hls_version":"` + v2 + `"}`, 200, true, "hls/" + v2},
		{"to the old version", `{"hls_version":"` + v1 + `"}`, 200, true, "hls/" + v1},
		{"to the same version", `{"hls_version":"` + v1 + `"}`, 200, false, "hls/" + v1},
		{"to a version that never committed", `{"hls_version":"` + uuid.NewString() + `"}`, 404, false, "hls/" + v1},
	}
	for _, s := range steps {
		code, body := e.setDelivery(t, clip, s.body)
		if code != s.code {
			t.Fatalf("%s: %d %v", s.name, code, body)
		}
		if code == 200 && data(body)["changed"] != s.changed {
			t.Fatalf("%s: changed = %v", s.name, data(body)["changed"])
		}
		if code == 404 && errType(body) != "version_not_found" {
			t.Fatalf("%s: %v", s.name, body)
		}
		if got := e.deliveryState(t, clip); got != s.state {
			t.Fatalf("%s: clip %s, want %s", s.name, got, s.state)
		}
	}
	if typ, key := e.playKey(t, clip, ""); typ != "hls" || key != "clips/"+clip+"/hls/"+v1+"/master.m3u8" {
		t.Fatalf("after rollback the clip plays %s %s", typ, key)
	}
	var rolledBack int
	e.s.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'delivery_rolled_back' AND entity_id = $1`, clip).Scan(&rolledBack)
	if rolledBack != 3 {
		t.Fatalf("delivery_rolled_back audited %d times, want 3 (one per change)", rolledBack)
	}

	for _, bad := range []string{`{}`, `{"source_type":"hls"}`, `{"hls_version":"nope"}`,
		`{"source_type":"mp4","hls_version":"` + v1 + `"}`} {
		if code, body := e.setDelivery(t, clip, bad); code != 400 || errType(body) != "validation_error" {
			t.Errorf("body %s: %d %v", bad, code, body)
		}
	}
	if code, body := e.setDelivery(t, uuid.NewString(), `{"source_type":"mp4"}`); code != 404 || errType(body) != "not_found" {
		t.Errorf("unknown clip: %d %v", code, body)
	}
}

// Rollback always wins: it cancels the active job, and that attempt's commit is fenced out.
func TestRollbackCancelsTheRunningAttempt(t *testing.T) {
	ctx := context.Background()
	e := setupAPI(t)
	clip := e.seedClip(t, "c", time.Now(), []byte("x"))
	rec, body := e.post(t, "/api/v1/admin/clips/"+clip+"/transcode")
	if rec.Code != 202 {
		t.Fatalf("transcode: %d %v", rec.Code, body)
	}
	a, err := transcode.Claim(ctx, e.s.Pool, data(body)["job_id"].(string)) // a worker picks it up
	if err != nil {
		t.Fatal(err)
	}

	if code, body := e.setDelivery(t, clip, `{"source_type":"mp4"}`); code != 200 || data(body)["changed"] != false {
		t.Fatalf("rollback: %d %v", code, body) // already mp4 — but the job is still cancelled
	}
	var status, reason string
	e.s.Pool.QueryRow(ctx, `SELECT status, failure_reason FROM transcode_jobs WHERE id = $1`, a.JobID).Scan(&status, &reason)
	if status != "failed" || reason != "manual" {
		t.Fatalf("job %s/%s, want failed/manual", status, reason)
	}
	var cancelled int
	e.s.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'job_cancelled' AND entity_id = $1
		AND metadata->>'from_status' = 'processing'`, a.JobID).Scan(&cancelled)
	if cancelled != 1 {
		t.Fatalf("job_cancelled audited %d times", cancelled)
	}

	res := transcode.Result{Renditions: []transcode.Rendition{{Rung: transcode.Rung{Height: 360}, Width: 640, Segments: 15}}}
	if err := transcode.Commit(ctx, e.s.Pool, a, res); !errors.Is(err, transcode.ErrSuperseded) {
		t.Fatalf("the cancelled attempt's commit = %v, want ErrSuperseded", err)
	}
	if got := e.deliveryState(t, clip); got != "mp4/" {
		t.Fatalf("clip %s after the fenced-out commit", got)
	}
}

// §12 #21 for the M5 routes: a non-admin gets 404, exactly like a route that does not exist.
func TestAdminRoutesHiddenFromNonAdmins(t *testing.T) {
	e := setupAPI(t)
	clip := e.seedClip(t, "c", time.Now(), []byte("x"))
	routes := []struct{ path, body string }{
		{"/api/v1/admin/clips/" + clip + "/transcode", ""},
		{"/api/v1/admin/clips/" + clip + "/delivery", `{"source_type":"mp4"}`},
	}
	if _, err := e.s.Pool.Exec(context.Background(), `UPDATE users SET is_admin = false WHERE id = $1`, e.s.DevUserID); err != nil {
		t.Fatal(err)
	}
	status := "/api/v1/admin/clips/" + clip + "/status"
	for _, r := range routes {
		if rec, body := e.postJSON(t, r.path, r.body); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("non-admin %s: %d %v", r.path, rec.Code, body)
		}
	}
	if rec, body := e.get(t, status, true); rec.Code != 404 || errType(body) != "not_found" {
		t.Errorf("non-admin status: %d %v", rec.Code, body)
	}
	var jobs int
	e.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM transcode_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Fatalf("a non-admin created %d jobs", jobs)
	}

	// Production mounts no admin route at all, admin or not: it runs no worker (decision 14).
	if _, err := e.s.Pool.Exec(context.Background(), `UPDATE users SET is_admin = true WHERE id = $1`, e.s.DevUserID); err != nil {
		t.Fatal(err)
	}
	e.s.Cfg.AppEnv = "prod"
	prod := apiEnv{e.s, e.s.Routes(), e.cookie}
	for _, r := range routes {
		if rec, body := prod.postJSON(t, r.path, r.body); rec.Code != 404 || errType(body) != "not_found" {
			t.Errorf("prod %s: %d %v", r.path, rec.Code, body)
		}
	}
	if rec, body := prod.get(t, status, true); rec.Code != 404 || errType(body) != "not_found" {
		t.Errorf("prod status: %d %v", rec.Code, body)
	}
}

// GET /admin/clips/{id}/status: the pointer, the newest job, committed versions newest first.
func TestClipStatus(t *testing.T) {
	ctx := context.Background()
	e := setupAPI(t)
	status := func(clip string) (int, map[string]any) {
		rec, body := e.get(t, "/api/v1/admin/clips/"+clip+"/status", true)
		return rec.Code, body
	}

	// A plain mp4 clip: no job, no versions — [] not null.
	plain := e.seedClip(t, "plain", time.Now(), []byte("x"))
	code, body := status(plain)
	d := data(body)
	if code != 200 || d["source_type"] != "mp4" || d["hls_version"] != nil || d["latest_job"] != nil {
		t.Fatalf("plain clip: %d %v", code, body)
	}
	if v, ok := d["versions"].([]any); !ok || len(v) != 0 {
		t.Fatalf("plain clip versions = %#v, want []", d["versions"])
	}

	// Two committed versions (v2 with two rungs, live), an older done job, a newer failed one.
	clip := e.seedClip(t, "c", time.Now(), []byte("x"))
	v1, v2 := e.version(t, clip), uuid.NewString()
	if _, err := e.s.Pool.Exec(ctx, `UPDATE renditions SET created_at = now() - interval '1 hour' WHERE hls_version = $1`, v1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Pool.Exec(ctx, `
		INSERT INTO renditions (clip_id, hls_version, height, width, video_bitrate, max_rate, audio_bitrate, bandwidth, segment_count)
		VALUES ($1, $2, 360, 640, 600000, 642000, 96000, 774900, 15), ($1, $2, 720, 1280, 2400000, 2568000, 128000, 2965600, 15)`,
		clip, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Pool.Exec(ctx, `UPDATE clips SET source_type = 'hls', hls_version = $2 WHERE id = $1`, clip, v2); err != nil {
		t.Fatal(err)
	}
	var failedJob string
	if err := e.s.Pool.QueryRow(ctx, `
		WITH old AS (INSERT INTO transcode_jobs (clip_id, status, created_at) VALUES ($1, 'done', now() - interval '1 hour'))
		INSERT INTO transcode_jobs (clip_id, status, attempts, failure_reason) VALUES ($1, 'failed', 3, 'probe_failed')
		RETURNING id::text`, clip).Scan(&failedJob); err != nil {
		t.Fatal(err)
	}

	code, body = status(clip)
	d = data(body)
	if code != 200 || d["source_type"] != "hls" || d["hls_version"] != v2 {
		t.Fatalf("status: %d %v", code, body)
	}
	j, _ := d["latest_job"].(map[string]any)
	if j["id"] != failedJob || j["status"] != "failed" || j["attempts"] != 3.0 || j["failure_reason"] != "probe_failed" || j["updated_at"] == nil {
		t.Fatalf("latest_job = %v, want the failed job %s", j, failedJob)
	}
	vs, _ := d["versions"].([]any)
	if len(vs) != 2 {
		t.Fatalf("versions = %v, want 2", vs)
	}
	first, second := vs[0].(map[string]any), vs[1].(map[string]any)
	if first["hls_version"] != v2 || second["hls_version"] != v1 || first["created_at"] == nil {
		t.Fatalf("versions order %v, %v; want v2 then v1", first["hls_version"], second["hls_version"])
	}
	rs := first["renditions"].([]any)
	top := rs[0].(map[string]any)
	if len(rs) != 2 || top["height"] != 720.0 || top["width"] != 1280.0 || top["video_bitrate"] != 2400000.0 ||
		top["bandwidth"] != 2965600.0 || top["segment_count"] != 15.0 || rs[1].(map[string]any)["height"] != 360.0 {
		t.Fatalf("v2 renditions = %v, want 720p then 360p with all five fields", rs)
	}
	if n := len(second["renditions"].([]any)); n != 1 {
		t.Fatalf("v1 has %d renditions, want 1", n)
	}

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		if code, body := status(id); code != 404 || errType(body) != "not_found" {
			t.Errorf("status %s: %d %v", id, code, body)
		}
	}
}

// GET /clips carries a presigned thumbnail_url once a transcode committed a thumbnail, and
// none before.
func TestListClipsThumbnail(t *testing.T) {
	e := setupAPI(t)
	with := e.seedClip(t, "with", time.Now(), []byte("x"))
	without := e.seedClip(t, "without", time.Now().Add(-time.Minute), []byte("x"))
	key := "clips/" + with + "/hls/" + uuid.NewString() + "/thumb.jpg"
	if _, err := e.s.Pool.Exec(context.Background(), `UPDATE clips SET thumbnail_key = $2 WHERE id = $1`, with, key); err != nil {
		t.Fatal(err)
	}
	rec, body := e.get(t, "/api/v1/clips", true)
	if rec.Code != 200 {
		t.Fatalf("clips: %d %v", rec.Code, body)
	}
	for _, it := range data(body)["items"].([]any) {
		item := it.(map[string]any)
		thumb, has := item["thumbnail_url"].(string)
		switch item["id"] {
		case with:
			u, err := url.Parse(thumb)
			if !has || err != nil || !strings.HasSuffix(u.Path, key) || u.Query().Get("X-Amz-Signature") == "" {
				t.Errorf("thumbnail_url %q, want a presigned URL for %s", thumb, key)
			}
		case without:
			if has {
				t.Errorf("clip without a thumbnail has thumbnail_url %q", thumb)
			}
		}
	}
}
