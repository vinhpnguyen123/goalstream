package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/vinhpnguyen123/goalstream/internal/audit"
	"github.com/vinhpnguyen123/goalstream/internal/config"
	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

// Failure reasons a worker writes (IMPLEMENTATION_PLAN §6). worker_crash belongs to the reaper
// and manual to the admin rollback.
const (
	ReasonDownloadFailed  = "download_failed"
	ReasonProbeFailed     = "probe_failed"
	ReasonFFmpegError     = "ffmpeg_error"
	ReasonTimeout         = "timeout"
	ReasonUploadFailed    = "upload_failed"
	ReasonCommitFailed    = "commit_failed" // the fenced commit rolled back (added 2026-10-26)
	ReasonPanic           = "panic"
	ReasonShutdown        = "shutdown"
	ReasonMissingOriginal = "missing_original" // terminal ↓: computed from the input, the same every time
	ReasonNoVideoStream   = "no_video_stream"
	ReasonNoAudioStream   = "no_audio_stream"
	ReasonBadLadder       = "bad_ladder"
)

// Everything under a version prefix is immutable — playlists included. The only mutable
// pointer is clips.hls_version, served by the uncached API.
const immutable = "public, max-age=604800, immutable"

// failTimeout bounds the failure handler, which runs on a fresh context: the attempt's may
// already be cancelled, and the failure must still be recorded.
const failTimeout = 10 * time.Second

var (
	// ErrNotClaimable: the job is processing elsewhere, done, failed, or does not exist.
	ErrNotClaimable = errors.New("job is not claimable")
	// ErrSuperseded: this attempt lost its fence (reaped, cancelled, superseded) and published nothing.
	ErrSuperseded = errors.New("attempt superseded")
)

// Failure is why an attempt ended without a commit. Terminal reasons are computed from the
// input and fail the job outright; every other reason retries until MAX_TRANSCODE_ATTEMPTS —
// a worker cannot tell a corrupt file from a disk that filled up mid-encode (D14).
type Failure struct {
	Reason   string
	Terminal bool
	Err      error
}

func (f *Failure) Error() string { return f.Reason + ": " + f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

func retryable(reason string, err error) *Failure { return &Failure{Reason: reason, Err: err} }
func terminal(reason string, err error) *Failure {
	return &Failure{Reason: reason, Terminal: true, Err: err}
}

// Attempt is one claim of a job. Version is minted fresh at the claim: it names the attempt's
// private prefix hls/<Version>/ AND is its fencing token.
type Attempt struct {
	JobID, ClipID, Version string
	Number                 int // attempts after this claim
}

// Result is what an attempt produced and uploaded, master playlist last.
type Result struct {
	Renditions []Rendition
	ThumbKey   string
}

// Claim takes a queued or retryable job: attempts+1 (a crash counts as an attempt), a fresh
// output_version, and the started_at clock the reaper reads. The guard is the claim:
// whoever matches the row owns the attempt; everyone else gets ErrNotClaimable.
func Claim(ctx context.Context, pool *pgxpool.Pool, jobID string) (Attempt, error) {
	a := Attempt{JobID: jobID}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE transcode_jobs
			   SET status = 'processing', attempts = attempts + 1, output_version = gen_random_uuid(),
			       started_at = now(), updated_at = now(), failure_reason = NULL, error = NULL
			 WHERE id = $1 AND status IN ('queued', 'retryable')
			RETURNING clip_id::text, attempts, output_version::text`, jobID).Scan(&a.ClipID, &a.Number, &a.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotClaimable
		}
		if err != nil {
			return fmt.Errorf("claiming job %s: %w", jobID, err)
		}
		return audit.Write(ctx, tx, audit.Entry{EntityType: "job", EntityID: jobID, Action: "transcode_started",
			ActorType: "worker", Metadata: map[string]any{"attempt": a.Number, "output_version": a.Version, "source": "worker"}})
	})
	return a, err
}

// Worker runs attempts. One at a time per process; several processes are safe because every
// write that ends an attempt is fenced.
type Worker struct {
	Pool  *pgxpool.Pool
	Store *storage.Client
	Cfg   config.WorkerConfig
	Log   *slog.Logger
	// put uploads one object; nil means Store.PutFile. A test seam that records upload order.
	put func(ctx context.Context, key, path, contentType, cacheControl string) error
	// hook is a test seam (§12 #31, #32), called at named steps of an attempt: "uploaded"
	// (everything but the master) and "mastered" (master written, commit next). nil in production.
	hook func(step string)
}

func (w *Worker) at(step string) {
	if w.hook != nil {
		w.hook(step)
	}
}

// Process runs one job end to end: claim → attempt → fenced commit, or the failure handler.
// TRANSCODE_TIMEOUT bounds steps 4–11 — the commit included, so a commit waiting on a lock
// cannot hold the job forever. nil means the clip now plays the new version. ErrNotClaimable
// and ErrSuperseded mean another owner decides the job. A *Failure has been recorded on the
// row; any other error means recording it failed too (Postgres unreachable) and the job stays
// processing until the reaper calls it stuck.
func (w *Worker) Process(ctx context.Context, jobID string) error {
	a, err := Claim(ctx, w.Pool, jobID)
	if err != nil {
		return err
	}
	log := w.Log.With("job", a.JobID, "clip", a.ClipID, "attempt", a.Number, "output_version", a.Version)
	log.Info("transcode started")

	jobCtx, cancel := context.WithTimeout(ctx, w.Cfg.TranscodeTimeout)
	defer cancel()
	res, err := w.attempt(jobCtx, a)
	if err == nil {
		err = Commit(jobCtx, w.Pool, a, res)
		switch {
		case err == nil:
			log.Info("transcode committed", "renditions", len(res.Renditions))
			return nil
		case errors.Is(err, ErrSuperseded):
			log.Warn("transcode superseded: fenced out, prefix left as an orphan")
			return err
		}
		// The commit rolled back — or its outcome is unknown (connection lost at COMMIT). Fail
		// is fenced: if the commit did land, the job is done and Fail matches 0 rows.
		err = retryable(ReasonCommitFailed, err)
	}
	var f *Failure
	if !errors.As(err, &f) {
		f = retryable(ReasonFFmpegError, err) // every path returns *Failure; this is a backstop
	}
	if !f.Terminal {
		switch {
		case ctx.Err() != nil: // the caller stopped us (SIGTERM, Ctrl-C): not this job's fault
			f.Reason = ReasonShutdown
		case errors.Is(jobCtx.Err(), context.DeadlineExceeded):
			f.Reason = ReasonTimeout
		}
	}
	if ferr := Fail(ctx, w.Pool, a, f, w.Cfg.MaxAttempts); ferr != nil {
		log.Error("recording the failure failed; the job stays processing", "reason", f.Reason, "err", ferr)
		return ferr
	}
	log.Warn("transcode attempt failed", "reason", f.Reason, "terminal", f.Terminal)
	return f
}

// attempt is steps 4–10 of IMPLEMENTATION_PLAN §9, with panics turned into a failure. No
// transaction is open while it runs. ctx carries the attempt's TRANSCODE_TIMEOUT.
func (w *Worker) attempt(ctx context.Context, a Attempt) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = Result{}, retryable(ReasonPanic, fmt.Errorf("panic: %v", p))
		}
	}()
	return w.run(ctx, a)
}

func (w *Worker) run(ctx context.Context, a Attempt) (Result, error) {
	var prefix string
	if err := w.Pool.QueryRow(ctx, `SELECT s3_prefix FROM clips WHERE id = $1`, a.ClipID).Scan(&prefix); err != nil {
		return Result{}, retryable(ReasonDownloadFailed, fmt.Errorf("reading clip %s: %w", a.ClipID, err))
	}
	dir, err := os.MkdirTemp(w.Cfg.WorkDir, "transcode-*")
	if err != nil {
		return Result{}, retryable(ReasonDownloadFailed, fmt.Errorf("creating work dir: %w", err))
	}
	defer os.RemoveAll(dir)

	// 4. The mezzanine. Missing means the durability rule was broken outside the app.
	src := filepath.Join(dir, "original.mp4")
	if err := w.download(ctx, prefix+"original.mp4", src); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Result{}, terminal(ReasonMissingOriginal, err)
		}
		return Result{}, retryable(ReasonDownloadFailed, err)
	}

	// 5. Re-probe: encode what is actually there, never what the clips row says.
	p, err := ProbeFile(ctx, src)
	if err != nil {
		return Result{}, retryable(ReasonProbeFailed, err)
	}
	if !p.HasVideo {
		return Result{}, terminal(ReasonNoVideoStream, errors.New("original has no video stream"))
	}
	if !p.HasAudio {
		return Result{}, terminal(ReasonNoAudioStream, errors.New("original has no audio stream"))
	}
	bps := p.VideoBps
	if bps == 0 && p.AudioBps > 0 && p.FormatBps > p.AudioBps {
		bps = p.FormatBps - p.AudioBps // the stream reports no bit_rate: container minus audio
	}
	if bps == 0 {
		return Result{}, retryable(ReasonProbeFailed, errors.New("no video bitrate reported"))
	}

	// 6. Never above the source.
	rungs, err := Ladder(bps, p.Height)
	if err != nil {
		return Result{}, terminal(ReasonBadLadder, fmt.Errorf("%d bps at %dp: %w", bps, p.Height, err))
	}

	// 7–8. Encode every rung, then the thumbnail.
	out := filepath.Join(dir, "hls")
	var res Result
	for _, r := range rungs {
		rd, err := EncodeRendition(ctx, src, out, r)
		if err != nil {
			return Result{}, retryable(ReasonFFmpegError, err)
		}
		res.Renditions = append(res.Renditions, rd)
	}
	if err := Thumbnail(ctx, src, filepath.Join(out, "thumb.jpg"), p.DurationMS); err != nil {
		return Result{}, retryable(ReasonFFmpegError, err)
	}

	// 9. Every segment, rendition playlist and the thumbnail — into THIS attempt's prefix only.
	base := prefix + "hls/" + a.Version + "/"
	if err := w.uploadTree(ctx, out, base); err != nil {
		return Result{}, retryable(ReasonUploadFailed, err)
	}
	w.at("uploaded")

	// 10. master.m3u8 LAST, only after everything it points at exists (L1): no master, no version.
	master := filepath.Join(dir, "master.m3u8")
	if err := os.WriteFile(master, MasterPlaylist(res.Renditions), 0o644); err != nil {
		return Result{}, retryable(ReasonUploadFailed, err)
	}
	if err := w.putFile(ctx, base+"master.m3u8", master, contentType(master), immutable); err != nil {
		return Result{}, retryable(ReasonUploadFailed, err)
	}
	w.at("mastered")
	res.ThumbKey = base + "thumb.jpg"
	return res, nil
}

func (w *Worker) download(ctx context.Context, key, dst string) error {
	body, err := w.Store.Open(ctx, key, "")
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return fmt.Errorf("downloading %s: %w", key, err)
	}
	return f.Close()
}

// uploadTree uploads every file under dir to base+<relative path>, UPLOAD_CONCURRENCY at a
// time. The first failure cancels the rest.
func (w *Worker) uploadTree(ctx context.Context, dir, base string) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.Cfg.UploadConcurrency)
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		key := base + filepath.ToSlash(rel)
		g.Go(func() error { return w.putFile(gctx, key, path, contentType(path), immutable) })
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}
	return walkErr
}

func (w *Worker) putFile(ctx context.Context, key, path, ct, cc string) error {
	if w.put != nil {
		return w.put(ctx, key, path, ct, cc)
	}
	return w.Store.PutFile(ctx, key, path, ct, cc)
}

func contentType(path string) string {
	switch filepath.Ext(path) {
	case ".ts":
		return "video/mp2t"
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".jpg":
		return "image/jpeg"
	}
	return "application/octet-stream"
}

// Commit is the fenced commit (IMPLEMENTATION_PLAN §9 step 11, L2) — the ONLY way a version
// goes live. One short transaction, in this order and no other:
//
//	a. the fence: job processing → done, guarded by THIS attempt's output_version. 0 rows means
//	   the attempt was reaped, cancelled or superseded: roll back, publish nothing;
//	b. lock the clip row and read its delivery state (for the audit);
//	c. record the renditions of this version;
//	d. point the clip at this version;
//	e. audit.
//
// The fence comes first so that nothing else in the transaction runs for an attempt that no
// longer owns the job. A superseded attempt's prefix stays in the bucket as an inert orphan.
func Commit(ctx context.Context, pool *pgxpool.Pool, a Attempt, res Result) error {
	superseded := false
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE transcode_jobs SET status = 'done', updated_at = now()
			 WHERE id = $1 AND status = 'processing' AND output_version = $2`, a.JobID, a.Version)
		if err != nil {
			return fmt.Errorf("fencing job %s: %w", a.JobID, err)
		}
		if tag.RowsAffected() == 0 {
			superseded = true
			return ErrSuperseded // rolls back
		}

		var fromType string
		var fromVersion *string
		if err := tx.QueryRow(ctx, `SELECT source_type, hls_version::text FROM clips WHERE id = $1 FOR UPDATE`,
			a.ClipID).Scan(&fromType, &fromVersion); err != nil {
			return fmt.Errorf("locking clip %s: %w", a.ClipID, err)
		}

		ladder := make([]string, 0, len(res.Renditions))
		for _, r := range res.Renditions {
			if _, err := tx.Exec(ctx, `
				INSERT INTO renditions (clip_id, hls_version, height, width, video_bitrate, max_rate,
				                        audio_bitrate, bandwidth, segment_count)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				a.ClipID, a.Version, r.Height, r.Width, r.VideoBps, r.MaxRate, r.AudioBps, r.Bandwidth, r.Segments); err != nil {
				return fmt.Errorf("recording %s rendition: %w", r.Dir(), err)
			}
			ladder = append(ladder, fmt.Sprintf("%s@%dk", r.Dir(), r.VideoBps/1000))
		}

		// $2 is fresh per attempt, so 0 rows is impossible unless the clip vanished.
		tag, err = tx.Exec(ctx, `
			UPDATE clips SET source_type = 'hls', hls_version = $2, thumbnail_key = $3
			 WHERE id = $1 AND hls_version IS DISTINCT FROM $2`, a.ClipID, a.Version, res.ThumbKey)
		if err != nil {
			return fmt.Errorf("pointing clip %s at %s: %w", a.ClipID, a.Version, err)
		}
		if n := tag.RowsAffected(); n != 1 {
			return fmt.Errorf("pointing clip %s at %s: %d rows, want 1", a.ClipID, a.Version, n)
		}

		if err := audit.Write(ctx, tx, audit.Entry{EntityType: "job", EntityID: a.JobID, Action: "transcode_completed",
			ActorType: "worker", Metadata: map[string]any{"output_version": a.Version, "ladder": strings.Join(ladder, ","),
				"segment_count": res.Renditions[0].Segments, "source": "worker"}}); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{EntityType: "clip", EntityID: a.ClipID, Action: "delivery_migrated",
			ActorType: "worker", Metadata: map[string]any{
				"from":   map[string]any{"source_type": fromType, "hls_version": fromVersion},
				"to":     map[string]any{"source_type": "hls", "hls_version": a.Version},
				"source": "worker"}})
	})
	if superseded {
		if aerr := audit.Write(ctx, pool, audit.Entry{EntityType: "job", EntityID: a.JobID, Action: "transcode_superseded",
			ActorType: "worker", Metadata: map[string]any{"attempt": a.Number, "output_version": a.Version, "source": "worker"}}); aerr != nil {
			return errors.Join(ErrSuperseded, aerr)
		}
	}
	return err
}

// Fail is the failure handler: one guarded statement, fenced like the commit, on a fresh
// context so a cancelled attempt can still record why it ended. The job goes to failed when the
// reason is terminal or attempts ≥ max, else retryable (the reaper re-pushes it).
//
// Shutdown does not burn an attempt — a deploy that restarts the worker three times must not
// kill a healthy job — so it never fails on the attempt limit and gives its attempt back.
// 0 rows: superseded, someone else decides the job; nothing to record.
func Fail(ctx context.Context, pool *pgxpool.Pool, a Attempt, f *Failure, maxAttempts int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failTimeout)
	defer cancel()
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var attempts int
		err := tx.QueryRow(ctx, `
			UPDATE transcode_jobs
			   SET status = CASE WHEN $4 OR ($3 <> 'shutdown' AND attempts >= $5) THEN 'failed' ELSE 'retryable' END,
			       attempts = CASE WHEN $3 = 'shutdown' THEN attempts - 1 ELSE attempts END,
			       failure_reason = $3, error = $6, updated_at = now()
			 WHERE id = $1 AND status = 'processing' AND output_version = $2
			RETURNING status, attempts`,
			a.JobID, a.Version, f.Reason, f.Terminal, maxAttempts, string(Tail([]byte(f.Err.Error())))).Scan(&status, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("recording failure of job %s: %w", a.JobID, err)
		}
		e := audit.Entry{EntityType: "job", EntityID: a.JobID, Action: "transcode_attempt_failed", ActorType: "worker",
			Metadata: map[string]any{"attempt": a.Number, "reason": f.Reason, "source": "worker"}}
		if status == "failed" {
			e.Action, e.Metadata = "transcode_failed", map[string]any{"attempts": attempts, "reason": f.Reason, "source": "worker"}
		}
		if err := audit.Write(ctx, tx, e); err != nil {
			return err
		}
		if f.Reason == ReasonMissingOriginal { // never auto-fixed: a human must restore the mezzanine
			return audit.Write(ctx, tx, audit.Entry{EntityType: "clip", EntityID: a.ClipID, Action: "invariant_violation",
				ActorType: "worker", Metadata: map[string]any{"kind": ReasonMissingOriginal, "detail": f.Err.Error()}})
		}
		return nil
	})
}
