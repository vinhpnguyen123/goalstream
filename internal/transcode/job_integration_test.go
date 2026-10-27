//go:build integration

package transcode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/db"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
	"github.com/vinhpnguyen123/goalstream/internal/storage/storagetest"
)

type jobEnv struct {
	pool *pgxpool.Pool
	cfg  config.Config
	w    *Worker

	mu       sync.Mutex
	uploaded []string // keys in upload-completion order
}

func setupJobs(t *testing.T) *jobEnv {
	t.Helper()
	ctx := context.Background()
	cfg := storagetest.Config(t) // empties the test bucket
	url := os.Getenv("TEST_DATABASE_URL")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE transcode_jobs, renditions, clips, matches, audit_log CASCADE`); err != nil {
		t.Fatal(err)
	}
	e := &jobEnv{pool: pool, cfg: cfg}
	store := storage.New(cfg.S3)
	e.w = &Worker{Pool: pool, Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cfg: config.WorkerConfig{WorkDir: t.TempDir(), TranscodeTimeout: 5 * time.Minute, MaxAttempts: 3, UploadConcurrency: 4}}
	e.w.put = func(ctx context.Context, key, path, ct, cc string) error {
		if err := store.PutFile(ctx, key, path, ct, cc); err != nil {
			return err
		}
		e.mu.Lock()
		e.uploaded = append(e.uploaded, key)
		e.mu.Unlock()
		return nil
	}
	return e
}

// clip inserts a clip whose original.mp4 is the fixture (none when fixture is "").
func (e *jobEnv) clip(t *testing.T, fixture string) (id, prefix string) {
	t.Helper()
	ctx := context.Background()
	id = uuid.NewString()
	prefix = "clips/" + id + "/"
	match := uuid.NewString()
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO matches (id, soccernet_path, home_team, away_team, competition, season)
		VALUES ($1, $2, 'Home', 'Away', 'test_league', '2015-2016')`, match, "test/"+match); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO clips (id, match_id, half, position_ms, event_type, title, match_minute, s3_prefix,
		                   width, height, source_bitrate, duration_ms, embedding)
		VALUES ($1, $2, 1, 60000, 'Goal', 'Home vs Away — Goal 1''', 1, $3, 1, 1, 1, 60000,
		        array_fill(0.1::real, ARRAY[128])::vector)`, id, match, prefix); err != nil {
		t.Fatal(err)
	}
	if fixture != "" {
		if err := e.w.Store.PutFile(ctx, prefix+"original.mp4", clipFixture(t, fixture), "video/mp4", ""); err != nil {
			t.Fatal(err)
		}
	}
	return id, prefix
}

func (e *jobEnv) job(t *testing.T, clipID string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO transcode_jobs (clip_id) VALUES ($1) RETURNING id::text`, clipID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type jobRow struct {
	Status, Reason, Error, Version string
	Attempts                       int
}

func (e *jobEnv) jobRow(t *testing.T, id string) jobRow {
	t.Helper()
	var r jobRow
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status, coalesce(failure_reason, ''), coalesce(error, ''), coalesce(output_version::text, ''), attempts
		  FROM transcode_jobs WHERE id = $1`, id).Scan(&r.Status, &r.Reason, &r.Error, &r.Version, &r.Attempts); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *jobEnv) delivery(t *testing.T, clipID string) (sourceType, version string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT source_type, coalesce(hls_version::text, '') FROM clips WHERE id = $1`, clipID).Scan(&sourceType, &version); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *jobEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *jobEnv) head(t *testing.T, key string) *s3.HeadObjectOutput {
	t.Helper()
	out, err := storage.RawClient(e.cfg.S3, e.cfg.S3.Endpoint).HeadObject(context.Background(),
		&s3.HeadObjectInput{Bucket: aws.String(e.cfg.S3.Bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("head %s: %v", key, err)
	}
	return out
}

func (e *jobEnv) read(t *testing.T, key string) []byte {
	t.Helper()
	body, err := e.w.Store.Open(context.Background(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// §12 #23: rungs == Ladder(measured), master uploaded last, the clip points at this attempt's
// version, renditions rows match master BANDWIDTH, every object immutable.
func TestProcessHappyPath(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, prefix := e.clip(t, "clip_1080p.mp4")
	jobID := e.job(t, clipID)

	if err := e.w.Process(ctx, jobID); err != nil {
		t.Fatal(err)
	}

	j := e.jobRow(t, jobID)
	if j.Status != "done" || j.Attempts != 1 {
		t.Fatalf("job = %+v, want done after 1 attempt", j)
	}
	st, ver := e.delivery(t, clipID)
	if st != "hls" || ver != j.Version {
		t.Fatalf("clip = %s/%s, want hls/%s", st, ver, j.Version)
	}
	base := prefix + "hls/" + j.Version + "/"

	p, err := ProbeFile(ctx, clipFixture(t, "clip_1080p.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := Ladder(p.VideoBps, p.Height)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(ctx, `SELECT height, bandwidth, segment_count FROM renditions
		WHERE clip_id = $1 AND hls_version = $2 ORDER BY height DESC`, clipID, j.Version)
	if err != nil {
		t.Fatal(err)
	}
	var heights, bandwidths []int
	for rows.Next() {
		var h, b, segs int
		if err := rows.Scan(&h, &b, &segs); err != nil {
			t.Fatal(err)
		}
		if segs != 15 { // 60 s / 4 s
			t.Errorf("%dp: %d segments, want 15", h, segs)
		}
		heights, bandwidths = append(heights, h), append(bandwidths, b)
	}
	if len(heights) != len(want) {
		t.Fatalf("renditions %v, want the %d rungs of Ladder(%d, %d)", heights, len(want), p.VideoBps, p.Height)
	}
	for i, r := range want {
		if heights[i] != r.Height || bandwidths[i] != r.Bandwidth {
			t.Errorf("rendition %d = %dp/%d, want %dp/%d", i, heights[i], bandwidths[i], r.Height, r.Bandwidth)
		}
	}
	if got := masterBandwidths(t, e.read(t, base+"master.m3u8")); !equalInts(got, bandwidths) {
		t.Errorf("master BANDWIDTH %v, renditions rows %v", got, bandwidths)
	}

	// L1: the master is the last object written, and nothing escaped this attempt's prefix.
	if last := e.uploaded[len(e.uploaded)-1]; last != base+"master.m3u8" {
		t.Fatalf("last upload %s, want the master", last)
	}
	for _, k := range e.uploaded {
		if !strings.HasPrefix(k, base) {
			t.Fatalf("uploaded %s outside %s", k, base)
		}
	}
	if cc := aws.ToString(e.head(t, base+"720p/seg_00000.ts").CacheControl); cc != immutable {
		t.Errorf("segment Cache-Control %q", cc)
	}
	if ct := aws.ToString(e.head(t, base+"master.m3u8").ContentType); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("master Content-Type %q", ct)
	}
	var thumb string
	e.pool.QueryRow(ctx, `SELECT thumbnail_key FROM clips WHERE id = $1`, clipID).Scan(&thumb)
	if thumb != base+"thumb.jpg" {
		t.Errorf("thumbnail_key %q", thumb)
	}
	e.head(t, thumb)

	for _, a := range []struct{ entity, id, action string }{
		{"job", jobID, "transcode_started"}, {"job", jobID, "transcode_completed"}, {"clip", clipID, "delivery_migrated"},
	} {
		if n := e.count(t, `SELECT count(*) FROM audit_log WHERE entity_type=$1 AND entity_id=$2 AND action=$3`, a.entity, a.id, a.action); n != 1 {
			t.Errorf("audit %s: %d rows", a.action, n)
		}
	}
}

// §12 #24: a re-transcode writes a NEW prefix and never touches the live one.
func TestRetranscodeNewPrefix(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, prefix := e.clip(t, "clip_576p.mp4")

	job1 := e.job(t, clipID)
	if err := e.w.Process(ctx, job1); err != nil {
		t.Fatal(err)
	}
	v1 := e.jobRow(t, job1).Version
	keys1, err := e.w.Store.ListKeys(ctx, prefix+"hls/"+v1+"/")
	if err != nil {
		t.Fatal(err)
	}
	etags := map[string]string{}
	for _, k := range keys1 {
		etags[k] = aws.ToString(e.head(t, k).ETag)
	}

	job2 := e.job(t, clipID)
	if err := e.w.Process(ctx, job2); err != nil {
		t.Fatal(err)
	}
	v2 := e.jobRow(t, job2).Version
	if _, ver := e.delivery(t, clipID); v2 == v1 || ver != v2 {
		t.Fatalf("versions %s → %s, clip at %s", v1, v2, ver)
	}
	keys1after, _ := e.w.Store.ListKeys(ctx, prefix+"hls/"+v1+"/")
	if len(keys1after) != len(keys1) {
		t.Fatalf("old prefix had %d objects, now %d", len(keys1), len(keys1after))
	}
	for k, tag := range etags {
		if got := aws.ToString(e.head(t, k).ETag); got != tag {
			t.Errorf("%s changed: ETag %s → %s", k, tag, got)
		}
	}
	if n := e.count(t, `SELECT count(DISTINCT hls_version) FROM renditions WHERE clip_id = $1`, clipID); n != 2 {
		t.Errorf("renditions for %d versions, want 2 (the old one stays a rollback target)", n)
	}
}

// The zombie, without timing: attempt 1 finishes its uploads after the job was re-claimed
// (what the M6 reaper does to a stuck job). Attempt 2 commits; attempt 1's commit matches
// 0 rows and publishes nothing — and a cancelled job fences out the same way.
func TestCommitIsFenced(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, prefix := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)

	a1, err := Claim(ctx, e.pool, jobID)
	if err != nil {
		t.Fatal(err)
	}
	res1, err := e.w.attempt(ctx, a1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE transcode_jobs SET status = 'retryable' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err) // the reaper's verdict on a1
	}
	a2, err := Claim(ctx, e.pool, jobID)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := e.w.attempt(ctx, a2)
	if err != nil {
		t.Fatal(err)
	}
	// The zombie commits WHILE attempt 2 is still processing: status alone cannot tell them
	// apart — only output_version can.
	if err := Commit(ctx, e.pool, a1, res1); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("zombie commit = %v, want ErrSuperseded", err)
	}
	if err := Commit(ctx, e.pool, a2, res2); err != nil {
		t.Fatal(err)
	}
	if err := Fail(ctx, e.pool, a1, retryable(ReasonFFmpegError, errors.New("late")), 3); err != nil {
		t.Fatal(err) // a zombie's failure is fenced too: no-op
	}
	if st, ver := e.delivery(t, clipID); st != "hls" || ver != a2.Version {
		t.Fatalf("clip = %s/%s, want attempt 2's %s", st, ver, a2.Version)
	}
	if j := e.jobRow(t, jobID); j.Status != "done" || j.Version != a2.Version || j.Reason != "" {
		t.Fatalf("job = %+v after the zombie", j)
	}
	if n := e.count(t, `SELECT count(*) FROM renditions WHERE hls_version = $1`, a1.Version); n != 0 {
		t.Fatalf("zombie recorded %d renditions", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'transcode_superseded' AND entity_id = $1`, jobID); n != 1 {
		t.Fatalf("transcode_superseded audited %d times", n)
	}
	e.head(t, prefix+"hls/"+a1.Version+"/master.m3u8") // the orphan stays: nothing is deleted

	// Cancelled mid-attempt (the M5 rollback does this): the commit is fenced out the same way.
	job2 := e.job(t, clipID)
	a3, err := Claim(ctx, e.pool, job2)
	if err != nil {
		t.Fatal(err)
	}
	res3, err := e.w.attempt(ctx, a3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE transcode_jobs SET status = 'failed', failure_reason = 'manual' WHERE id = $1`, job2); err != nil {
		t.Fatal(err)
	}
	if err := Commit(ctx, e.pool, a3, res3); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("commit after cancel = %v", err)
	}
	if _, ver := e.delivery(t, clipID); ver != a2.Version {
		t.Fatalf("cancelled attempt moved the clip to %s", ver)
	}
}

// Terminal reasons fail the job on the first attempt and leave the clip playing as before.
func TestTerminalFailures(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	for _, tc := range []struct{ fixture, reason string }{
		{"clip_noaudio.mp4", ReasonNoAudioStream},
		{"", ReasonMissingOriginal}, // the mezzanine was deleted behind the app's back
	} {
		clipID, _ := e.clip(t, tc.fixture)
		jobID := e.job(t, clipID)
		var f *Failure
		if err := e.w.Process(ctx, jobID); !errors.As(err, &f) || f.Reason != tc.reason || !f.Terminal {
			t.Fatalf("%s: Process = %v, want terminal %s", tc.reason, err, tc.reason)
		}
		if j := e.jobRow(t, jobID); j.Status != "failed" || j.Reason != tc.reason || j.Attempts != 1 {
			t.Fatalf("%s: job = %+v", tc.reason, j)
		}
		if st, ver := e.delivery(t, clipID); st != "mp4" || ver != "" {
			t.Fatalf("%s: clip = %s/%s, want untouched mp4", tc.reason, st, ver)
		}
		if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action='transcode_failed' AND entity_id=$1`, jobID); n != 1 {
			t.Fatalf("%s: transcode_failed audited %d times", tc.reason, n)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action='invariant_violation' AND metadata->>'kind'='missing_original'`); n != 1 {
		t.Fatalf("invariant_violation audited %d times, want 1", n)
	}
}

// Retryable reasons retry until attempts ≥ MAX; then the job is terminal and never claimed again.
func TestRetriesUntilMax(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, _ := e.clip(t, "clip_corrupt.mp4")
	jobID := e.job(t, clipID)
	for attempt := 1; attempt <= 3; attempt++ {
		var f *Failure
		if err := e.w.Process(ctx, jobID); !errors.As(err, &f) || f.Reason != ReasonProbeFailed {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		j := e.jobRow(t, jobID)
		wantStatus := map[bool]string{true: "failed", false: "retryable"}[attempt == 3]
		if j.Status != wantStatus || j.Attempts != attempt || j.Reason != ReasonProbeFailed {
			t.Fatalf("after attempt %d: %+v, want %s", attempt, j, wantStatus)
		}
		if j.Error == "" || len(j.Error) > stderrTail {
			t.Fatalf("error column is %d bytes", len(j.Error))
		}
	}
	if err := e.w.Process(ctx, jobID); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("a failed job was claimable: %v", err)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatalf("clip is %s after a failed transcode", st)
	}
}

// SIGTERM after the master upload, before the commit: the commit runs under the stopped
// context and rolls back, and the failure handler still records it — shutdown, attempt given
// back even at the attempt limit — so the job is claimable again instead of stuck processing.
func TestShutdownBeforeCommit(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	if _, err := e.pool.Exec(ctx, `UPDATE transcode_jobs SET attempts = 2 WHERE id = $1`, jobID); err != nil {
		t.Fatal(err) // this claim makes attempts 3 = MAX
	}
	stopped, sigterm := context.WithCancel(ctx)
	put := e.w.put
	e.w.put = func(ctx context.Context, key, path, ct, cc string) error {
		err := put(ctx, key, path, ct, cc)
		if strings.HasSuffix(key, "master.m3u8") {
			sigterm()
		}
		return err
	}
	var f *Failure
	if err := e.w.Process(stopped, jobID); !errors.As(err, &f) || f.Reason != ReasonShutdown {
		t.Fatalf("Process = %v, want shutdown", err)
	}
	if j := e.jobRow(t, jobID); j.Status != "retryable" || j.Attempts != 2 || j.Reason != ReasonShutdown {
		t.Fatalf("job = %+v, want retryable with attempts back to 2", j)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatal("a rolled-back commit published")
	}

	e.w.put = put // the restarted worker finishes it
	if err := e.w.Process(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delivery(t, clipID); st != "hls" {
		t.Fatalf("clip is %s after the retry", st)
	}
}

// TRANSCODE_TIMEOUT bounds the commit too: a commit stuck behind a lock on the clip row times
// out, rolls back and is recorded, instead of holding the job processing forever.
func TestCommitLockWaitTimesOut(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM clips WHERE id = $1 FOR UPDATE`, clipID); err != nil {
		t.Fatal(err)
	}
	e.w.Cfg.TranscodeTimeout = 40 * time.Second // the encode fits; the commit then waits out the rest
	start := time.Now()
	var f *Failure
	if err := e.w.Process(ctx, jobID); !errors.As(err, &f) || f.Reason != ReasonTimeout {
		t.Fatalf("Process = %v, want timeout", err)
	}
	if took := time.Since(start); took > e.w.Cfg.TranscodeTimeout+10*time.Second {
		t.Fatalf("Process took %s: the commit was not bounded", took)
	}
	if j := e.jobRow(t, jobID); j.Status != "retryable" || j.Attempts != 1 {
		t.Fatalf("job = %+v, want retryable", j)
	}
	if n := e.count(t, `SELECT count(*) FROM renditions WHERE clip_id = $1`, clipID); n != 0 {
		t.Fatalf("%d renditions survived the rollback", n)
	}
	tx.Rollback(ctx)

	e.w.Cfg.TranscodeTimeout = 5 * time.Minute
	if err := e.w.Process(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delivery(t, clipID); st != "hls" {
		t.Fatalf("clip is %s after the lock was released", st)
	}
}

// A timeout and a recovered panic are retryable failures, not crashes.
func TestTimeoutAndPanic(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")

	e.w.Cfg.TranscodeTimeout = time.Millisecond
	jobID := e.job(t, clipID)
	var f *Failure
	if err := e.w.Process(ctx, jobID); !errors.As(err, &f) || f.Reason != ReasonTimeout {
		t.Fatalf("Process = %v, want timeout", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE transcode_jobs SET status = 'failed' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}

	e.w.Cfg.TranscodeTimeout = 5 * time.Minute
	put := e.w.put
	e.w.put = func(ctx context.Context, key, path, ct, cc string) error {
		if strings.HasSuffix(key, "master.m3u8") {
			panic("boom")
		}
		return put(ctx, key, path, ct, cc)
	}
	jobID = e.job(t, clipID)
	if err := e.w.Process(ctx, jobID); !errors.As(err, &f) || f.Reason != ReasonPanic {
		t.Fatalf("Process = %v, want panic", err)
	}
	if j := e.jobRow(t, jobID); j.Status != "retryable" {
		t.Fatalf("job = %+v after a panic", j)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatal("a panicking attempt published")
	}
}

// Exactly one of many concurrent claims wins (run with -race).
func TestConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, _ := e.clip(t, "")
	jobID := e.job(t, clipID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Claim(ctx, e.pool, jobID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrNotClaimable):
				lost++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 15 {
		t.Fatalf("won %d lost %d, want 1/15", won, lost)
	}
	if j := e.jobRow(t, jobID); j.Attempts != 1 || j.Status != "processing" {
		t.Fatalf("job = %+v", j)
	}
}

func masterBandwidths(t *testing.T, master []byte) []int {
	t.Helper()
	var out []int
	sc := bufio.NewScanner(bytes.NewReader(master))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "#EXT-X-STREAM-INF:BANDWIDTH=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.SplitN(rest, ",", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Rollback and a fenced commit racing on one clip, in both orders: rollback always wins — if
// the commit lands first the rollback flips it back, if the rollback lands first the commit is
// fenced out — and the two never deadlock (jobs are locked before the clip in both). -race, 3×.
func TestRollbackRacesCommit(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	for i := 0; i < 20; i++ {
		clipID, _ := e.clip(t, "")
		a, err := Claim(ctx, e.pool, e.job(t, clipID))
		if err != nil {
			t.Fatal(err)
		}
		res := Result{Renditions: []Rendition{{Rung: Rung{Height: 360, VideoBps: 600_000}, Width: 640, Segments: 15}},
			ThumbKey: "clips/" + clipID + "/hls/" + a.Version + "/thumb.jpg"}
		var wg sync.WaitGroup
		var commitErr, rollbackErr error
		wg.Add(2)
		go func() { defer wg.Done(); commitErr = Commit(ctx, e.pool, a, res) }()
		go func() { defer wg.Done(); _, rollbackErr = SetDelivery(ctx, e.pool, clipID, "") }()
		wg.Wait()
		if rollbackErr != nil || (commitErr != nil && !errors.Is(commitErr, ErrSuperseded)) {
			t.Fatalf("round %d: commit %v, rollback %v", i, commitErr, rollbackErr)
		}
		if st, ver := e.delivery(t, clipID); st != "mp4" {
			t.Fatalf("round %d: clip %s/%s — the rollback lost", i, st, ver)
		}
	}
}

// A job inserted but not yet committed when a rollback starts (CreateJob in flight): the
// rollback waits for it and cancels it. Without the per-clip lock the cancel cannot see the
// uncommitted row, the job commits queued, and running it later undoes the rollback.
func TestRollbackWaitsForAnUncommittedJob(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	clipID, _ := e.clip(t, "")

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := lockClipJobs(ctx, tx, clipID); err != nil { // exactly what CreateJob holds
		t.Fatal(err)
	}
	var jobID string
	if err := tx.QueryRow(ctx, `INSERT INTO transcode_jobs (clip_id) VALUES ($1) RETURNING id::text`, clipID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := SetDelivery(ctx, e.pool, clipID, ""); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("rollback finished (%v) while a job insert was still uncommitted", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rollback still blocked after the insert committed")
	}
	if j := e.jobRow(t, jobID); j.Status != "failed" || j.Reason != "manual" {
		t.Fatalf("job = %+v: the rollback missed the job created alongside it", j)
	}
}
