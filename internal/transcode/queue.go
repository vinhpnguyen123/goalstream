package transcode

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Queue is the Redis side of the job queue (IMPLEMENTATION_PLAN §9). It holds no truth: every
// entry names a transcode_jobs row, the claim in Postgres decides who runs it, and the reaper
// rebuilds both lists from Postgres when Redis forgets them.
type Queue struct {
	R  *redis.Client
	NS string // key namespace: "transcode" in production, unique per test
}

func NewQueue(r *redis.Client) Queue { return Queue{R: r, NS: "transcode"} }

func (q Queue) pending() string    { return q.NS + ":queue" }
func (q Queue) processing() string { return q.NS + ":processing" }

// Push enqueues a job. Call it only after the row is committed: an entry whose row is not
// claimable yet is dropped by the first worker that takes it.
func (q Queue) Push(ctx context.Context, jobID string) error {
	return q.R.LPush(ctx, q.pending(), jobID).Err()
}

// Next moves the oldest entry onto the processing list (BLMOVE RIGHT → LEFT), blocking up to
// block. "" means nothing arrived in time.
func (q Queue) Next(ctx context.Context, block time.Duration) (string, error) {
	id, err := q.R.BLMove(ctx, q.pending(), q.processing(), "RIGHT", "LEFT", block).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return id, err
}

// Ack removes one entry of the job from the processing list (LREM).
func (q Queue) Ack(ctx context.Context, jobID string) error {
	return q.R.LRem(ctx, q.processing(), 1, jobID).Err()
}

const (
	nextBlock  = 5 * time.Second // short, so a shutdown is noticed between jobs
	maxBackoff = 30 * time.Second
	ackTimeout = 5 * time.Second
)

// Loop is the worker loop (IMPLEMENTATION_PLAN §9 steps 1 and 12) around Worker.Process: one
// job at a time per process. Several loops, in one process or many, are safe — the claim picks
// one owner per job and every write that ends an attempt is fenced.
type Loop struct {
	Q   Queue
	W   *Worker
	Log *slog.Logger
}

// Run takes jobs until ctx is cancelled (SIGTERM), then returns once the job in hand has ended.
// That job keeps running for SHUTDOWN_GRACE after the signal; then its context is cancelled,
// ffmpeg is killed by CommandContext, and Process records reason shutdown — the attempt is
// given back, so a deploy that restarts the worker never burns one.
func (l *Loop) Run(ctx context.Context) {
	jobs, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(l.W.Cfg.ShutdownGrace, cancel) })
	defer stop()

	var backoff time.Duration
	for ctx.Err() == nil {
		// jobs, not ctx: a BLMOVE cut off mid-reply could move an entry we never see.
		id, err := l.Q.Next(jobs, nextBlock)
		if err != nil {
			backoff = min(max(2*backoff, time.Second), maxBackoff)
			l.Log.Error("queue unavailable", "err", err, "retry_in", backoff.String())
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		if id == "" {
			continue
		}
		if ctx.Err() != nil {
			// Taken after the signal: hand it back unclaimed. Its row is still queued, so the
			// reaper re-pushes it after REQUEUE_AFTER.
			l.ack(id)
			return
		}
		l.process(jobs, id)
	}
}

func (l *Loop) process(ctx context.Context, id string) {
	err := l.W.Process(ctx, id)
	if err == nil {
		l.W.at("committed")
	}
	var f *Failure
	switch {
	case err == nil, errors.Is(err, ErrSuperseded), errors.As(err, &f):
		// Process logged the outcome and the row records it.
	case errors.Is(err, ErrNotClaimable):
		// A duplicate entry, or a job someone else holds or already finished.
		l.Log.Info("job not claimable, dropping its queue entry", "job", id)
	default:
		// Claim or the failure handler could not reach Postgres, or the id is garbage. If a
		// claim landed, the row stays processing until the reaper calls it stuck.
		l.Log.Error("job error, dropping its queue entry", "job", id, "err", err)
	}
	// Every outcome drops the entry: Postgres says what happens next, and the reaper re-pushes
	// whatever is still queued or retryable.
	l.ack(id)
}

// ack runs on a fresh context: the job's may already be cancelled by the shutdown.
func (l *Loop) ack(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := l.Q.Ack(ctx, id); err != nil {
		// The reaper's hygiene sweep removes it once the row is no longer processing.
		l.Log.Error("removing the processing entry failed", "job", id, "err", err)
	}
}
