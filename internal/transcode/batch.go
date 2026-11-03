package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pending lists the clips the catalogue migration (M7) still has to transcode, oldest first:
// mp4 clips that never had a live version and have no active job. limit <= 0 means all.
//
// hls_version IS NULL leaves out a clip an admin rolled back to mp4 — the rollback keeps the
// version as a re-promotion target, and a batch must not silently undo that decision. A clip
// whose last job failed stays in, so re-running the batch retries it.
func Pending(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	var lim *int // NULL: LIMIT ALL
	if limit > 0 {
		lim = &limit
	}
	rows, err := pool.Query(ctx, `
		SELECT c.id::text FROM clips c
		 WHERE c.source_type = 'mp4' AND c.hls_version IS NULL
		   AND NOT EXISTS (SELECT 1 FROM transcode_jobs j
		                    WHERE j.clip_id = c.id AND j.status IN ('queued', 'processing', 'retryable'))
		 ORDER BY c.created_at, c.id
		 LIMIT $1`, lim)
	if err != nil {
		return nil, fmt.Errorf("listing pending clips: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Enqueue creates and pushes one job per clip — CreateJob + Push like the admin endpoint, so the
// row is committed before Redis hears of it, but each clip's eligibility is re-checked inside
// the job's transaction (createJob): Pending's list is a snapshot. A clip that gained an active
// job, was migrated or rolled back, or vanished since Pending is skipped. A failed push is logged, not fatal: the row is the job,
// and the reaper pushes it after REQUEUE_AFTER. On any other error it stops and returns the
// jobs created so far — they are real and will run.
func Enqueue(ctx context.Context, pool *pgxpool.Pool, q Queue, clips []string, log *slog.Logger) ([]string, error) {
	var jobs []string
	for _, clip := range clips {
		job, err := createJob(ctx, pool, clip, true)
		var active *ActiveJobError
		switch {
		case errors.As(err, &active), errors.Is(err, ErrClipNotFound), errors.Is(err, ErrNotPending):
			log.Info("skipping clip", "clip", clip, "err", err)
			continue
		case err != nil:
			return jobs, err
		}
		jobs = append(jobs, job)
		if err := q.Push(ctx, job); err != nil {
			log.Warn("queue push failed; the reaper will push the job", "job", job, "err", err)
		}
	}
	return jobs, nil
}

// Report is where a batch of jobs stands. Attempts and FirstTry count finished jobs only.
type Report struct {
	Active, Done, Failed int
	FirstTry             int            // done on attempt 1
	Attempts             map[int]int    // attempts → finished jobs
	Reasons              map[string]int // failure_reason → failed jobs
}

// Progress reads the batch's jobs. The batch is over when Active is 0.
func Progress(ctx context.Context, pool *pgxpool.Pool, jobs []string) (Report, error) {
	r := Report{Attempts: map[int]int{}, Reasons: map[string]int{}}
	rows, err := pool.Query(ctx, `
		SELECT status, attempts, coalesce(failure_reason, '') FROM transcode_jobs WHERE id = ANY($1::uuid[])`, jobs)
	if err != nil {
		return r, fmt.Errorf("reading batch jobs: %w", err)
	}
	var status, reason string
	var attempts int
	_, err = pgx.ForEachRow(rows, []any{&status, &attempts, &reason}, func() error {
		switch status {
		case "done":
			r.Done++
			if attempts == 1 {
				r.FirstTry++
			}
		case "failed":
			r.Failed++
			r.Reasons[reason]++
		default:
			r.Active++
			return nil
		}
		r.Attempts[attempts]++
		return nil
	})
	return r, err
}
