package transcode

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
)

var (
	// ErrClipNotFound: no clip with that id.
	ErrClipNotFound = errors.New("clip not found")
	// ErrVersionNotFound: the rollback target has no renditions rows — it never committed.
	ErrVersionNotFound = errors.New("version not found")
)

// ActiveJobError: the clip already has a queued, processing or retryable job.
type ActiveJobError struct{ JobID string }

func (e *ActiveJobError) Error() string { return "job " + e.JobID + " is already active" }

// CreateJob queues a transcode of the clip — also when it is already hls: that is a
// re-transcode and produces a new version. The partial unique index jobs_one_active_per_clip is
// the guard: two active jobs would race the hls_version pointer. M5 does not push to Redis; the
// job is run by hand (worker -once). M6 pushes after this row exists — Postgres first.
func CreateJob(ctx context.Context, pool *pgxpool.Pool, clipID string) (string, error) {
	var jobID string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := lockClipJobs(ctx, tx, clipID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO transcode_jobs (clip_id) VALUES ($1) RETURNING id::text`, clipID).Scan(&jobID)
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{EntityType: "job", EntityID: jobID, Action: "job_created",
			ActorType: "admin", Metadata: map[string]any{"clip_id": clipID, "source": "admin"}})
	})
	var pg *pgconn.PgError
	switch {
	case err == nil:
		return jobID, nil
	case errors.As(err, &pg) && pg.Code == "23503": // transcode_jobs.clip_id → clips
		return "", ErrClipNotFound
	case errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "jobs_one_active_per_clip":
		var active string
		if qerr := pool.QueryRow(ctx, `SELECT id::text FROM transcode_jobs
			WHERE clip_id = $1 AND status IN ('queued', 'processing', 'retryable')`, clipID).Scan(&active); qerr != nil {
			return "", fmt.Errorf("reading the active job of clip %s: %w", clipID, qerr) // it finished in between
		}
		return "", &ActiveJobError{JobID: active}
	default:
		return "", fmt.Errorf("creating job for clip %s: %w", clipID, err)
	}
}

// SetDelivery is the admin rollback (IMPLEMENTATION_PLAN §9 ROLLBACK): point the clip at its
// original MP4 (version == "") or at a committed hls version. One transaction:
//
//  1. cancel the clip's active job, if any (→ failed, reason manual). A running attempt's fenced
//     commit then matches 0 rows: rollback always wins over an in-flight transcode;
//  2. the guarded flip. To mp4: only an hls clip moves; hls_version is KEPT (the CHECK allows it)
//     so the version stays a re-promotion target. To a version: guarded on BOTH columns — an mp4
//     clip still holding that same version is correctly flipped back — and only to a version
//     that has renditions rows, i.e. one that committed.
//
// lockClipJobs (first) serializes it with CreateJob on the same clip: without it, a job INSERTed
// but not yet committed is invisible to the cancel below, commits after it, and later runs —
// undoing the rollback through a perfectly valid fence (Codex review, 2026-10-27). Then jobs are
// locked before the clip, the same order as the fenced commit, so the two cannot deadlock. changed reports the flip only: a cancelled job stays cancelled even when the clip
// was already in the requested state. An unknown version rolls everything back.
func SetDelivery(ctx context.Context, pool *pgxpool.Pool, clipID, version string) (changed bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := lockClipJobs(ctx, tx, clipID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			UPDATE transcode_jobs j SET status = 'failed', failure_reason = 'manual', updated_at = now()
			  FROM (SELECT id, status FROM transcode_jobs
			         WHERE clip_id = $1 AND status IN ('queued', 'processing', 'retryable') FOR UPDATE) old
			 WHERE j.id = old.id
			RETURNING j.id::text, old.status`, clipID)
		if err != nil {
			return fmt.Errorf("cancelling jobs of clip %s: %w", clipID, err)
		}
		type cancelled struct{ id, from string }
		var jobs []cancelled
		var c cancelled
		if _, err := pgx.ForEachRow(rows, []any{&c.id, &c.from}, func() error { jobs = append(jobs, c); return nil }); err != nil {
			return fmt.Errorf("cancelling jobs of clip %s: %w", clipID, err)
		}
		for _, j := range jobs {
			if err := audit.Write(ctx, tx, audit.Entry{EntityType: "job", EntityID: j.id, Action: "job_cancelled",
				ActorType: "admin", Metadata: map[string]any{"from_status": j.from, "reason": "manual", "source": "admin"}}); err != nil {
				return err
			}
		}

		var fromType string
		var fromVersion *string
		err = tx.QueryRow(ctx, `SELECT source_type, hls_version::text FROM clips WHERE id = $1 FOR UPDATE`,
			clipID).Scan(&fromType, &fromVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClipNotFound
		}
		if err != nil {
			return fmt.Errorf("locking clip %s: %w", clipID, err)
		}

		var tag pgconn.CommandTag
		if version == "" {
			tag, err = tx.Exec(ctx, `UPDATE clips SET source_type = 'mp4' WHERE id = $1 AND source_type = 'hls'`, clipID)
		} else {
			tag, err = tx.Exec(ctx, `
				UPDATE clips SET source_type = 'hls', hls_version = $2
				 WHERE id = $1 AND NOT (source_type = 'hls' AND hls_version = $2)
				   AND EXISTS (SELECT 1 FROM renditions WHERE clip_id = $1 AND hls_version = $2)`, clipID, version)
		}
		if err != nil {
			return fmt.Errorf("flipping clip %s: %w", clipID, err)
		}
		if tag.RowsAffected() == 0 {
			already := fromType == "mp4" && version == "" ||
				fromType == "hls" && fromVersion != nil && *fromVersion == version
			if !already {
				return ErrVersionNotFound
			}
			return nil // the cancellations above still commit
		}
		changed = true
		toType, toVersion := "hls", fromVersion
		if version != "" {
			toVersion = &version
		} else {
			toType = "mp4"
		}
		return audit.Write(ctx, tx, audit.Entry{EntityType: "clip", EntityID: clipID, Action: "delivery_rolled_back",
			ActorType: "admin", Metadata: map[string]any{
				"from":   map[string]any{"source_type": fromType, "hls_version": fromVersion},
				"to":     map[string]any{"source_type": toType, "hls_version": toVersion},
				"reason": "manual", "source": "admin"}})
	})
	return changed, err
}

// lockClipJobs serializes the operations that create or cancel a clip's jobs — CreateJob and
// SetDelivery — for the rest of the transaction. It is taken FIRST, before any job or clip row,
// so the lock order stays advisory → jobs → clip; the fenced commit (jobs → clip) does not need
// it, because its job row lock already orders it against the cancel. Keys are hashtext, so two
// clips can share one: they only wait on each other, never see wrong data.
func lockClipJobs(ctx context.Context, tx pgx.Tx, clipID string) error {
	// $1::uuid::text: one key per clip, however the caller spelled its id — an uppercase and a
	// lowercase UUID name the same row but would hash to two locks (Codex review, 2026-11-02).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('clip-jobs:' || $1::uuid::text))`, clipID); err != nil {
		return fmt.Errorf("locking jobs of clip %s: %w", clipID, err)
	}
	return nil
}
