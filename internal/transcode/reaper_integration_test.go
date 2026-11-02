//go:build integration

package transcode

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"

	"github.com/redis/go-redis/v9"
)

func (e *jobEnv) reaper(q Queue) *Reaper {
	return &Reaper{Pool: e.pool, Q: q, Cfg: e.w.Cfg, Log: e.w.Log}
}

// startReaper runs the reaper (e.w.Cfg clocks) until the test ends.
func (e *jobEnv) startReaper(t *testing.T, q Queue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.reaper(q).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// backdate makes the job look old to the reaper: started_at for S1, updated_at for S2.
func (e *jobEnv) backdate(t *testing.T, jobID string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE transcode_jobs
		SET started_at = started_at - interval '2 hours', updated_at = updated_at - interval '2 hours' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
}

// setStatus puts a job in status; processing goes through Claim (jobs_processing_chk wants an
// output_version and a started_at).
func (e *jobEnv) setStatus(t *testing.T, jobID, status string) {
	t.Helper()
	ctx := context.Background()
	if status == "processing" {
		if _, err := Claim(ctx, e.pool, jobID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := e.pool.Exec(ctx, `UPDATE transcode_jobs SET status = $2 WHERE id = $1`, jobID, status); err != nil {
		t.Fatal(err)
	}
}

func (e *jobEnv) audits(t *testing.T, jobID, action string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = $2`, jobID, action)
}

// §12 #30: a corrupt original fails the same way every time. Driven only by the loop and the
// reaper — no manual push: 3 attempts, the retries re-pushed after REQUEUE_AFTER, final
// failed/probe_failed, the error tail bounded, the clip untouched.
func TestPipelineCorruptInput(t *testing.T) {
	e := setupJobs(t)
	q := e.queue(t)
	clipID, _ := e.clip(t, "clip_corrupt.mp4")
	jobID, err := CreateJob(context.Background(), e.pool, clipID)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Push(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	e.start(t, q)
	e.startReaper(t, q)

	j := e.waitStatus(t, jobID, "failed")
	if j.Attempts != 3 || j.Reason != ReasonProbeFailed || j.Error == "" || len(j.Error) > 8<<10 {
		t.Fatalf("job = %+v (error %d bytes), want 3 attempts, probe_failed, error ≤ 8 KiB", j, len(j.Error))
	}
	if a, f := e.audits(t, jobID, "transcode_attempt_failed"), e.audits(t, jobID, "transcode_failed"); a != 2 || f != 1 {
		t.Fatalf("attempt_failed %d, failed %d, want 2/1", a, f)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatalf("clip %s after a failed job", st)
	}
	waitEmpty(t, q)
}

// The push after the row failed (Redis down at POST time): the queued row alone is enough —
// the reaper pushes it and the loop publishes it.
func TestPipelineRecoversFailedPush(t *testing.T) {
	e := setupJobs(t)
	q := e.queue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID, err := CreateJob(context.Background(), e.pool, clipID) // and no push
	if err != nil {
		t.Fatal(err)
	}
	e.start(t, q)
	e.startReaper(t, q)
	if j := e.waitStatus(t, jobID, "done"); j.Attempts != 1 {
		t.Fatalf("job = %+v", j)
	}
}

// §12 #36: Redis forgets everything mid-run (the queue namespace's keys deleted — what FLUSHALL
// does to this queue, without wiping the dev Redis). Every job still completes, once.
func TestPipelineSurvivesRedisFlush(t *testing.T) {
	e := setupJobs(t)
	q := e.queue(t)
	ctx := context.Background()
	var jobs []string
	for range 3 {
		clipID, _ := e.clip(t, "clip_576p.mp4")
		jobID, err := CreateJob(ctx, e.pool, clipID)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Push(ctx, jobID); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, jobID)
	}
	e.start(t, q)
	e.startReaper(t, q)
	e.waitStatus(t, jobs[0], "processing")
	if err := q.R.Del(ctx, q.pending(), q.processing()).Err(); err != nil {
		t.Fatal(err)
	}
	for _, id := range jobs {
		if j := e.waitStatus(t, id, "done"); j.Attempts != 1 {
			t.Fatalf("job %s = %+v, want done after 1 attempt", id, j)
		}
	}
	waitEmpty(t, q)
}

// §12 #32: the zombie, through the reaper. Attempt 1 stalls after writing its master (laptop
// asleep); the reaper calls it stuck; attempt 2 publishes; attempt 1 wakes and commits — its
// fence matches 0 rows. The clip stays on attempt 2.
func TestZombieFencedThroughReaper(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	clipID, prefix := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)

	reached, release := make(chan struct{}), make(chan struct{})
	zombie := *e.w
	zombie.hook = func(step string) {
		if step == "mastered" {
			close(reached)
			<-release
		}
	}
	zerr := make(chan error, 1)
	go func() { zerr <- zombie.Process(ctx, jobID) }()
	<-reached
	a1 := e.jobRow(t, jobID)

	e.backdate(t, jobID)
	if n, err := e.reaper(q).Stuck(ctx); err != nil || n != 1 {
		t.Fatalf("Stuck = %d, %v", n, err)
	}
	if j := e.jobRow(t, jobID); j.Status != "retryable" || j.Reason != ReasonWorkerCrash || j.Attempts != 1 {
		t.Fatalf("reaped job = %+v", j)
	}
	if err := e.w.Process(ctx, jobID); err != nil { // attempt 2
		t.Fatal(err)
	}
	a2 := e.jobRow(t, jobID)

	close(release)
	if err := <-zerr; !errors.Is(err, ErrSuperseded) {
		t.Fatalf("zombie = %v, want ErrSuperseded", err)
	}
	if st, ver := e.delivery(t, clipID); st != "hls" || ver != a2.Version || ver == a1.Version {
		t.Fatalf("clip %s/%s, want attempt 2's %s", st, ver, a2.Version)
	}
	if j := e.jobRow(t, jobID); j.Status != "done" || j.Version != a2.Version {
		t.Fatalf("job = %+v after the zombie woke", j)
	}
	if e.audits(t, jobID, "transcode_superseded") != 1 || e.audits(t, jobID, "job_requeued") != 1 {
		t.Fatal("want one transcode_superseded and one job_requeued")
	}
	if n := e.count(t, `SELECT count(*) FROM renditions WHERE hls_version = $1`, a1.Version); n != 0 {
		t.Fatalf("zombie recorded %d renditions", n)
	}
	e.head(t, prefix+"hls/"+a1.Version+"/master.m3u8") // its orphan stays, unreferenced
}

// §12 #33: a job that kills its worker every time. A crash records nothing (runtime.Goexit: the
// goroutine dies, recover sees nil); each one counts as an attempt, and after MAX the reaper
// fails it with worker_crash. Never claimed again. A fresh processing job is left alone.
func TestPoisonPill(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	healthyClip, _ := e.clip(t, "")
	healthy := e.job(t, healthyClip)
	if _, err := Claim(ctx, e.pool, healthy); err != nil {
		t.Fatal(err)
	}

	crashing := *e.w
	crashing.hook = func(step string) {
		if step == "uploaded" {
			runtime.Goexit()
		}
	}
	for i := 1; i <= 3; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			crashing.Process(ctx, jobID)
		}()
		<-done
		if j := e.jobRow(t, jobID); j.Status != "processing" || j.Attempts != i {
			t.Fatalf("after crash %d: job = %+v, want processing (nothing recorded)", i, j)
		}
		e.backdate(t, jobID)
		if n, err := e.reaper(q).Stuck(ctx); err != nil || n != 1 {
			t.Fatalf("Stuck after crash %d = %d, %v", i, n, err)
		}
	}
	if j := e.jobRow(t, jobID); j.Status != "failed" || j.Reason != ReasonWorkerCrash || j.Attempts != 3 {
		t.Fatalf("job = %+v, want failed/worker_crash after 3", j)
	}
	if _, err := Claim(ctx, e.pool, jobID); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("claim after failed = %v", err)
	}
	if r, f := e.audits(t, jobID, "job_requeued"), e.audits(t, jobID, "transcode_failed"); r != 2 || f != 1 {
		t.Fatalf("job_requeued %d, transcode_failed %d, want 2/1", r, f)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatalf("clip %s", st)
	}
	if j := e.jobRow(t, healthy); j.Status != "processing" {
		t.Fatalf("a fresh processing job was reaped: %+v", j)
	}
}

// S2: only queued/retryable rows older than REQUEUE_AFTER are pushed, Postgres first, and an id
// already waiting is not pushed twice.
func TestReaperRepush(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	r := e.reaper(q)
	job := func(status string, old bool) string {
		c, _ := e.clip(t, "")
		id := e.job(t, c)
		e.setStatus(t, id, status)
		if old {
			e.backdate(t, id)
		}
		return id
	}
	oldQueued, oldRetryable := job("queued", true), job("retryable", true)
	job("queued", false)
	job("processing", true)
	job("done", true)
	job("failed", true)

	if n, err := r.Repush(ctx); err != nil || n != 2 {
		t.Fatalf("Repush = %d, %v, want 2", n, err)
	}
	got, _ := q.R.LRange(ctx, q.pending(), 0, -1).Result()
	slices.Sort(got)
	want := []string{oldQueued, oldRetryable}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM transcode_jobs WHERE id = ANY($1) AND updated_at > now() - interval '1 minute'`, want); n != 2 {
		t.Fatalf("updated_at bumped on %d rows, want 2", n)
	}

	// Still waiting behind a busy worker: old again, but already queued → no duplicate.
	e.backdate(t, oldQueued)
	e.backdate(t, oldRetryable)
	if n, err := r.Repush(ctx); err != nil || n != 0 {
		t.Fatalf("second Repush = %d, %v, want 0", n, err)
	}
	if l, _ := llen(t, q); l != 2 {
		t.Fatalf("queue length %d, want 2", l)
	}
}

// S3: processing-list entries whose row is not processing go; a live attempt's entry stays.
func TestReaperHygiene(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	ids := map[string]string{}
	for _, status := range []string{"processing", "done", "queued", "failed"} {
		c, _ := e.clip(t, "")
		id := e.job(t, c)
		e.setStatus(t, id, status)
		ids[status] = id
	}
	for _, id := range []string{ids["processing"], ids["done"], ids["done"], ids["queued"], ids["failed"], "not-a-uuid"} {
		if err := q.R.LPush(ctx, q.processing(), id).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.reaper(q).Hygiene(ctx); err != nil || n != 5 {
		t.Fatalf("Hygiene = %d, %v, want 5 removed", n, err)
	}
	if got, _ := q.R.LRange(ctx, q.processing(), 0, -1).Result(); !slices.Equal(got, []string{ids["processing"]}) {
		t.Fatalf("processing list = %v, want only the live attempt", got)
	}
}

// cancelAfterPushes cancels a context once n LPUSHes have gone through: a sweep whose deadline
// expires mid-pass.
type cancelAfterPushes struct {
	n      int
	cancel context.CancelFunc
}

func (h *cancelAfterPushes) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *cancelAfterPushes) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *cancelAfterPushes) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() == "lpush" {
			if h.n--; h.n == 0 {
				h.cancel()
			}
		}
		return err
	}
}

// A pass cut short leaves the rows it did not reach with their old clocks, so the next pass
// pushes exactly those — oldest first, nothing starved, nothing pushed twice.
func TestReaperRepushCutShort(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	var ids []string // oldest first
	for i := range 5 {
		c, _ := e.clip(t, "")
		id := e.job(t, c)
		if _, err := e.pool.Exec(ctx, `UPDATE transcode_jobs SET updated_at = now() - make_interval(hours => $2) WHERE id = $1`,
			id, 10-i); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	opt, _ := redis.ParseURL(e.cfg.RedisURL)
	slow := redis.NewClient(opt)
	t.Cleanup(func() { slow.Close() })
	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slow.AddHook(&cancelAfterPushes{n: 2, cancel: cancel})
	r := e.reaper(Queue{R: slow, NS: q.NS})

	if n, err := r.Repush(passCtx); n != 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cut-short Repush = %d, %v, want 2 and context.Canceled", n, err)
	}
	if got, _ := q.R.LRange(ctx, q.pending(), 0, -1).Result(); !slices.Equal(got, []string{ids[1], ids[0]}) {
		t.Fatalf("queue = %v, want the two oldest", got)
	}
	if n := e.count(t, `SELECT count(*) FROM transcode_jobs WHERE id = ANY($1) AND updated_at < now() - interval '1 hour'`, ids[2:]); n != 3 {
		t.Fatalf("%d of the 3 unreached rows kept their clock, want 3", n)
	}

	// The next pass: the 3 it missed, and no duplicate of the 2 it pushed (still fresh).
	if n, err := e.reaper(q).Repush(ctx); err != nil || n != 3 {
		t.Fatalf("next Repush = %d, %v, want 3", n, err)
	}
	got, _ := q.R.LRange(ctx, q.pending(), 0, -1).Result()
	slices.Sort(got)
	want := slices.Clone(ids)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want each job once", got)
	}
}
