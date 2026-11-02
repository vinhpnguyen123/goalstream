package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
	"github.com/vinhpnguyen123/goalstream/internal/config"
)

// ReasonWorkerCrash: the attempt recorded no outcome within JOB_STUCK_AFTER. Written by the reaper.
const ReasonWorkerCrash = "worker_crash"

// sweepTimeout bounds one pass of the three sweeps.
const sweepTimeout = 30 * time.Second

// Reaper is the queue's recovery (IMPLEMENTATION_PLAN §9 REAPER). Redis holds no truth, so
// everything it can lose — a push that failed, a flushed list, the entry of a worker that
// died — is rebuilt from Postgres. Safe to run in several workers at once: every write is
// guarded by the state it expects.
type Reaper struct {
	Pool *pgxpool.Pool
	Q    Queue
	Cfg  config.WorkerConfig
	Log  *slog.Logger
}

// Run sweeps now and then every REAPER_INTERVAL until ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	t := time.NewTicker(r.Cfg.ReaperInterval)
	defer t.Stop()
	for {
		r.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep runs S1 → S2 → S3 once. In that order: a job S1 just made retryable is not re-pushed
// until REQUEUE_AFTER has passed (S1 sets updated_at), which is the retry backoff.
func (r *Reaper) Sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	for _, s := range []struct {
		name string
		fn   func(context.Context) (int, error)
	}{{"stuck", r.Stuck}, {"repush", r.Repush}, {"hygiene", r.Hygiene}} {
		n, err := s.fn(ctx)
		if err != nil {
			r.Log.Error("reaper sweep failed", "sweep", s.name, "err", err)
		} else if n > 0 {
			r.Log.Info("reaper sweep", "sweep", s.name, "jobs", n)
		}
	}
}

// Stuck is S1: an attempt processing for longer than JOB_STUCK_AFTER has a dead worker (or a
// zombie — then the fence makes this harmless). It goes to retryable, or to failed with
// worker_crash once attempts ≥ MAX: a crash counts as an attempt, otherwise a file that kills
// the worker would loop forever.
//
// One statement guarded on status and started_at: under the row lock Postgres re-checks the
// WHERE against the latest row, so an attempt that committed, failed or was re-claimed in the
// meantime (new started_at) is not touched.
func (r *Reaper) Stuck(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE transcode_jobs
			   SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'retryable' END,
			       failure_reason = 'worker_crash', error = 'no outcome recorded within JOB_STUCK_AFTER',
			       updated_at = now()
			 WHERE status = 'processing' AND started_at < now() - make_interval(secs => $1)
			RETURNING id::text, status, attempts, floor(extract(epoch FROM now() - started_at))::int`,
			r.Cfg.JobStuckAfter.Seconds(), r.Cfg.MaxAttempts)
		if err != nil {
			return fmt.Errorf("reaping stuck jobs: %w", err)
		}
		type reaped struct {
			id, status         string
			attempts, stuckFor int
		}
		var out []reaped
		for rows.Next() {
			var j reaped
			if err := rows.Scan(&j.id, &j.status, &j.attempts, &j.stuckFor); err != nil {
				return err
			}
			out = append(out, j)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reaping stuck jobs: %w", err)
		}
		for _, j := range out {
			e := audit.Entry{EntityType: "job", EntityID: j.id, Action: "job_requeued", ActorType: "worker",
				Metadata: map[string]any{"stuck_for_seconds": j.stuckFor, "attempt": j.attempts, "to_status": j.status, "source": "reaper"}}
			if j.status == "failed" {
				e.Action, e.Metadata = "transcode_failed", map[string]any{"attempts": j.attempts, "reason": ReasonWorkerCrash, "source": "reaper"}
			}
			if err := audit.Write(ctx, tx, e); err != nil {
				return err
			}
		}
		n = len(out)
		return nil
	})
	return n, err
}

// Repush is S2: queued and retryable rows untouched for REQUEUE_AFTER are pushed again. This is
// what re-enqueues retryable jobs (REQUEUE_AFTER is the retry backoff), rescues a job whose
// first push failed, and rebuilds the queue after a Redis flush.
//
// Per row, oldest first, Postgres first: the row's updated_at is bumped (so the next sweep waits
// again), THEN the push — a row is claimable before its entry exists, never the reverse. A row
// is bumped only when its push is next, so a pass cut short (sweep deadline, Redis error) leaves
// every row it did not reach with its old clock, first in line for the next pass (Codex review,
// 2026-11-02: one batch UPDATE reset them all and could starve the tail). LPOS skips ids
// already queued, so a long wait behind a busy worker does not pile duplicates onto the list.
func (r *Reaper) Repush(ctx context.Context) (int, error) {
	after := r.Cfg.RequeueAfter.Seconds()
	rows, err := r.Pool.Query(ctx, `
		SELECT id::text FROM transcode_jobs
		 WHERE status IN ('queued', 'retryable') AND updated_at < now() - make_interval(secs => $1)
		 ORDER BY updated_at`, after)
	if err != nil {
		return 0, fmt.Errorf("selecting jobs to re-push: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("selecting jobs to re-push: %w", err)
	}
	pushed := 0
	for _, id := range ids {
		// Same guard as the SELECT: 0 rows means it was claimed, cancelled, or another reaper
		// got here first — not ours to push.
		tag, err := r.Pool.Exec(ctx, `
			UPDATE transcode_jobs SET updated_at = now()
			 WHERE id = $1 AND status IN ('queued', 'retryable') AND updated_at < now() - make_interval(secs => $2)`, id, after)
		if err != nil {
			return pushed, fmt.Errorf("bumping job %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		ok, err := r.Q.PushIfAbsent(ctx, id)
		if err != nil {
			// This row waits one REQUEUE_AFTER; the rows after it keep their clocks.
			return pushed, fmt.Errorf("re-pushing job %s: %w", id, err)
		}
		if ok {
			pushed++
		}
	}
	return pushed, nil
}

// Hygiene is S3: every processing-list entry whose row is not processing is removed — the
// entry of a job that committed but crashed before its LREM, or of a worker that died between
// BLMOVE and the claim. A worker that has BLMOVEd but not yet claimed can lose its entry here;
// harmless, Postgres is truth and S1 finds the row if that worker then dies.
func (r *Reaper) Hygiene(ctx context.Context) (int, error) {
	ids, err := r.Q.R.LRange(ctx, r.Q.processing(), 0, -1).Result()
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	// id::text, not $1::uuid[]: a garbage entry must not fail the whole sweep.
	rows, err := r.Pool.Query(ctx, `SELECT id::text FROM transcode_jobs WHERE id::text = ANY($1) AND status = 'processing'`, ids)
	if err != nil {
		return 0, fmt.Errorf("reading processing jobs: %w", err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("reading processing jobs: %w", err)
	}
	keep := map[string]bool{}
	for _, id := range live {
		keep[id] = true
	}
	removed := 0
	for _, id := range ids {
		if keep[id] {
			continue
		}
		n, err := r.Q.R.LRem(ctx, r.Q.processing(), 0, id).Result()
		if err != nil {
			return removed, fmt.Errorf("removing stale entry %s: %w", id, err)
		}
		removed += int(n)
	}
	return removed, nil
}

// PushIfAbsent pushes the job unless it is already waiting in the queue (LPOS). Not atomic: a
// duplicate slipping in between is harmless, the claim lets only one worker run it.
func (q Queue) PushIfAbsent(ctx context.Context, jobID string) (bool, error) {
	err := q.R.LPos(ctx, q.pending(), jobID, redis.LPosArgs{}).Err()
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, redis.Nil):
		return false, err
	}
	return true, q.Push(ctx, jobID)
}
