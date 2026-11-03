//go:build integration

package transcode

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Pending: never-migrated mp4 clips without an active job, oldest first, limited.
func TestPending(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	at := func(clip string, minutesAgo int) {
		if _, err := e.pool.Exec(ctx, `UPDATE clips SET created_at = now() - make_interval(mins => $2) WHERE id = $1`,
			clip, minutesAgo); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(sql string, args ...any) {
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	oldest, _ := e.clip(t, "")
	at(oldest, 50)
	newer, _ := e.clip(t, "")
	at(newer, 10)
	retry, _ := e.clip(t, "") // last job failed: a re-run must retry it
	at(retry, 30)
	exec(`INSERT INTO transcode_jobs (clip_id, status, attempts, failure_reason) VALUES ($1, 'failed', 3, 'probe_failed')`, retry)

	live, _ := e.clip(t, "") // already hls
	exec(`UPDATE clips SET source_type = 'hls', hls_version = $2 WHERE id = $1`, live, uuid.NewString())
	rolledBack, _ := e.clip(t, "") // mp4 again by an admin's choice, version kept
	exec(`UPDATE clips SET hls_version = $2 WHERE id = $1`, rolledBack, uuid.NewString())
	for _, status := range []string{"queued", "processing", "retryable"} {
		busy, _ := e.clip(t, "")
		if status == "processing" {
			exec(`INSERT INTO transcode_jobs (clip_id, status, output_version, started_at) VALUES ($1, 'processing', gen_random_uuid(), now())`, busy)
		} else {
			exec(`INSERT INTO transcode_jobs (clip_id, status) VALUES ($1, $2)`, busy, status)
		}
	}

	for limit, want := range map[int][]string{0: {oldest, retry, newer}, -1: {oldest, retry, newer}, 2: {oldest, retry}} {
		got, err := Pending(ctx, e.pool, limit)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Pending(%d) = %v, want %v", limit, got, want)
		}
	}
}

// A batch through the real loop: every job runs once, a clip with no original fails terminally
// and is reported, enqueuing twice queues nothing new, and afterwards only the failed clip is
// still pending.
func TestBatch(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var good []string
	for range 3 {
		id, _ := e.clip(t, "clip_576p.mp4")
		good = append(good, id)
	}
	missing, _ := e.clip(t, "") // no original.mp4 → missing_original, terminal

	clips, err := Pending(ctx, e.pool, 0)
	if err != nil || len(clips) != 4 {
		t.Fatalf("Pending = %v, %v; want 4 clips", clips, err)
	}
	jobs, err := Enqueue(ctx, e.pool, q, clips, log)
	if err != nil || len(jobs) != 4 {
		t.Fatalf("Enqueue = %v, %v; want 4 jobs", jobs, err)
	}
	if again, err := Enqueue(ctx, e.pool, q, clips, log); err != nil || len(again) != 0 {
		t.Fatalf("second Enqueue = %v, %v; want nothing (every clip has an active job)", again, err)
	}
	if p, _ := llen(t, q); p != 4 {
		t.Fatalf("%d entries queued, want 4", p)
	}
	if r, err := Progress(ctx, e.pool, jobs); err != nil || r.Active != 4 {
		t.Fatalf("before the loop: %+v, %v; want 4 active", r, err)
	}

	e.start(t, q)
	var r Report
	wait(t, 5*time.Minute, "the batch to finish", func() bool {
		r, err = Progress(ctx, e.pool, jobs)
		if err != nil {
			t.Fatal(err)
		}
		return r.Active == 0
	})
	want := Report{Done: 3, Failed: 1, FirstTry: 3,
		Attempts: map[int]int{1: 4}, Reasons: map[string]int{"missing_original": 1}}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("report = %+v, want %+v", r, want)
	}
	for _, id := range good {
		if st, ver := e.delivery(t, id); st != "hls" || ver == "" {
			t.Errorf("clip %s is %s/%q, want hls", id, st, ver)
		}
	}
	waitEmpty(t, q)

	if left, err := Pending(ctx, e.pool, 0); err != nil || !reflect.DeepEqual(left, []string{missing}) {
		t.Fatalf("Pending after the batch = %v, %v; want only the failed clip %s", left, err, missing)
	}
}

// Pending is a snapshot: a clip migrated, or migrated and rolled back, before Enqueue reaches it
// gets no job — re-transcoding it would undo the rollback. The admin's explicit CreateJob still
// re-transcodes. "commit during insert": the fenced commit of the clip's running job is in
// flight while Enqueue inserts; the insert waits for it, the re-check then sees the pointer.
func TestEnqueueRechecks(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	setup := func(t *testing.T) (*jobEnv, Queue, string) {
		e := setupJobs(t)
		clip, _ := e.clip(t, "")
		if got, err := Pending(ctx, e.pool, 0); err != nil || !reflect.DeepEqual(got, []string{clip}) {
			t.Fatalf("Pending = %v, %v; want [%s]", got, err, clip)
		}
		return e, e.queue(t), clip
	}
	exec := func(t *testing.T, e *jobEnv, sql string, args ...any) {
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	noNewJob := func(t *testing.T, e *jobEnv, q Queue, clip string, jobs []string, err error, before int) {
		t.Helper()
		if err != nil || len(jobs) != 0 {
			t.Fatalf("Enqueue = %v, %v; want no job", jobs, err)
		}
		if n := e.count(t, `SELECT count(*) FROM transcode_jobs WHERE clip_id = $1`, clip); n != before {
			t.Fatalf("%d jobs for the clip, want %d (no new row)", n, before)
		}
		if p, _ := llen(t, q); p != 0 {
			t.Fatalf("%d entries pushed, want 0", p)
		}
	}

	t.Run("rolled back after Pending", func(t *testing.T) {
		e, q, clip := setup(t)
		exec(t, e, `INSERT INTO transcode_jobs (clip_id, status, attempts) VALUES ($1, 'done', 1)`, clip)
		exec(t, e, `UPDATE clips SET hls_version = $2 WHERE id = $1`, clip, uuid.NewString()) // mp4, version kept
		jobs, err := Enqueue(ctx, e.pool, q, []string{clip}, log)
		noNewJob(t, e, q, clip, jobs, err, 1)

		if _, err := CreateJob(ctx, e.pool, clip); err != nil { // the admin may still choose to
			t.Fatalf("admin CreateJob on a rolled-back clip: %v", err)
		}
	})

	t.Run("migrated after Pending", func(t *testing.T) {
		e, q, clip := setup(t)
		exec(t, e, `UPDATE clips SET source_type = 'hls', hls_version = $2 WHERE id = $1`, clip, uuid.NewString())
		jobs, err := Enqueue(ctx, e.pool, q, []string{clip}, log)
		noNewJob(t, e, q, clip, jobs, err, 0)
	})

	t.Run("commit during insert", func(t *testing.T) {
		e, q, clip := setup(t)
		var job string
		if err := e.pool.QueryRow(ctx, `
			INSERT INTO transcode_jobs (clip_id, status, attempts, output_version, started_at)
			VALUES ($1, 'processing', 1, gen_random_uuid(), now()) RETURNING id::text`, clip).Scan(&job); err != nil {
			t.Fatal(err)
		}
		// The fenced commit's writes, in its order (job, then clip), held open.
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `UPDATE transcode_jobs SET status = 'done' WHERE id = $1`, job); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE clips SET source_type = 'hls', hls_version = $2 WHERE id = $1`, clip, uuid.NewString()); err != nil {
			t.Fatal(err)
		}

		type result struct {
			jobs []string
			err  error
		}
		done := make(chan result, 1)
		go func() { j, err := Enqueue(ctx, e.pool, q, []string{clip}, log); done <- result{j, err} }()
		select {
		case r := <-done:
			t.Fatalf("Enqueue returned %v, %v while the commit was still open", r.jobs, r.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			noNewJob(t, e, q, clip, r.jobs, r.err, 1)
		case <-time.After(10 * time.Second):
			t.Fatal("Enqueue still blocked after the commit")
		}
	})
}
