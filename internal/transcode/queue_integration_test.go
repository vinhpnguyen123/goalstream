//go:build integration

package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// queue is a Queue on the live Redis under a namespace of its own, deleted after the test.
func (e *jobEnv) queue(t *testing.T) Queue {
	t.Helper()
	opt, err := redis.ParseURL(e.cfg.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	q := Queue{R: rdb, NS: "test-" + uuid.NewString()}
	t.Cleanup(func() {
		rdb.Del(context.Background(), q.pending(), q.processing())
		rdb.Close()
	})
	return q
}

// start runs a loop until the returned stop is called; stop waits for Run to return.
func (e *jobEnv) start(t *testing.T, q Queue) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Loop{Q: q, W: e.w, Log: e.w.Log}).Run(ctx)
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("loop did not stop within a minute of the signal")
		}
	}
	t.Cleanup(stop)
	return stop
}

// wait polls until cond holds, failing after limit.
func wait(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *jobEnv) waitStatus(t *testing.T, jobID, status string) jobRow {
	t.Helper()
	var j jobRow
	wait(t, 3*time.Minute, "job "+status, func() bool { j = e.jobRow(t, jobID); return j.Status == status })
	return j
}

func llen(t *testing.T, q Queue) (pending, processing int64) {
	t.Helper()
	ctx := context.Background()
	var err1, err2 error
	pending, err1 = q.R.LLen(ctx, q.pending()).Result()
	processing, err2 = q.R.LLen(ctx, q.processing()).Result()
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	return
}

func waitEmpty(t *testing.T, q Queue) {
	t.Helper()
	wait(t, 30*time.Second, "both lists empty", func() bool { p, w := llen(t, q); return p == 0 && w == 0 })
}

// A pushed job is taken, published, and its entry removed.
func TestLoopRunsQueuedJob(t *testing.T) {
	e := setupJobs(t)
	q := e.queue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	if err := q.Push(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	e.start(t, q)

	j := e.waitStatus(t, jobID, "done")
	if st, ver := e.delivery(t, clipID); st != "hls" || ver != j.Version || j.Attempts != 1 {
		t.Fatalf("clip %s/%s, job %+v", st, ver, j)
	}
	waitEmpty(t, q)
}

// §12 #34: the same job delivered twice to two workers → one claim, one publish; the loser
// drops its entry and moves on. A garbage entry is dropped too.
func TestLoopDuplicateDelivery(t *testing.T) {
	e := setupJobs(t)
	q := e.queue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	ctx := context.Background()
	for _, id := range []string{jobID, jobID, "not-a-uuid"} {
		if err := q.Push(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	e.start(t, q)
	e.start(t, q)

	j := e.waitStatus(t, jobID, "done")
	waitEmpty(t, q)
	if j.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", j.Attempts)
	}
	for action, want := range map[string]int{"transcode_started": 1, "transcode_completed": 1, "transcode_superseded": 0} {
		if n := e.count(t, `SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = $2`, jobID, action); n != want {
			t.Errorf("%s audited %d times, want %d", action, n, want)
		}
	}
	if n := e.count(t, `SELECT count(DISTINCT hls_version) FROM renditions WHERE clip_id = $1`, clipID); n != 1 {
		t.Fatalf("%d versions recorded, want 1", n)
	}
}

// A job that finishes within SHUTDOWN_GRACE is published, not interrupted.
func TestLoopShutdownWithinGrace(t *testing.T) {
	e := setupJobs(t)
	q := e.queue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	if err := q.Push(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	e.w.Cfg.ShutdownGrace = 5 * time.Minute
	stop := e.start(t, q)
	e.waitStatus(t, jobID, "processing")
	stop() // returns only once the job has ended
	if j := e.jobRow(t, jobID); j.Status != "done" {
		t.Fatalf("job = %+v, want done within the grace", j)
	}
	waitEmpty(t, q)
}

// Redis down: the loop backs off without spinning and still stops promptly on the signal.
func TestLoopRedisDown(t *testing.T) {
	e := setupJobs(t)
	q := Queue{R: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1}), NS: "unused"}
	stop := e.start(t, q)
	time.Sleep(1500 * time.Millisecond) // two failures: 1 s then 2 s backoff
	begin := time.Now()
	stop()
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("stop took %s while backing off", d)
	}
}
