//go:build integration

package transcode

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

// etags lists every object under prefix with its ETag.
func (e *jobEnv) etags(t *testing.T, prefix string) map[string]string {
	t.Helper()
	cl := storage.RawClient(e.cfg.S3, e.cfg.S3.Endpoint)
	out := map[string]string{}
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: aws.String(e.cfg.S3.Bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Contents {
			out[aws.ToString(o.Key)] = aws.ToString(o.ETag)
		}
	}
	return out
}

func sameObjects(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// §12 #31, in-process: the worker dies (runtime.Goexit on the loop's goroutine — deferred
// cleanup runs, recover sees nil, nothing is recorded, the processing entry stays) after its
// uploads, after its master, and after its commit. Each time: the clip plays its previous
// version until a commit; a fresh worker + the reaper recover; exactly one new live version;
// no renditions for a dead attempt; every object of every prefix that was ever live unchanged.
func TestCrashMatrix(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	q := e.queue(t)
	clipID, prefix := e.clip(t, "clip_576p.mp4")

	// The live version the crashes must never touch.
	if err := e.w.Process(ctx, e.job(t, clipID)); err != nil {
		t.Fatal(err)
	}
	_, live := e.delivery(t, clipID)
	everLive := map[string]map[string]string{live: e.etags(t, prefix+"hls/"+live+"/")}

	for _, step := range []string{"uploaded", "mastered", "committed"} {
		t.Run(step, func(t *testing.T) {
			jobID, err := CreateJob(ctx, e.pool, clipID)
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Push(ctx, jobID); err != nil {
				t.Fatal(err)
			}
			crashed := make(chan struct{})
			e.w.hook = func(s string) {
				if s == step {
					close(crashed)
					runtime.Goexit()
				}
			}
			stop := e.start(t, q)
			select {
			case <-crashed:
			case <-time.After(3 * time.Minute):
				t.Fatalf("never reached %s", step)
			}
			stop() // its goroutine is already gone
			e.w.hook = nil

			dead := e.jobRow(t, jobID)
			_, ver := e.delivery(t, clipID)
			if _, w := llen(t, q); w != 1 {
				t.Fatalf("processing list has %d entries after the crash, want the dead worker's 1", w)
			}
			if step == "committed" {
				if dead.Status != "done" || ver != dead.Version {
					t.Fatalf("crash after commit: job %+v, clip on %s", dead, ver)
				}
			} else if dead.Status != "processing" || ver != live {
				t.Fatalf("crash before commit: job %+v, clip on %s, want processing and still %s", dead, ver, live)
			}

			// Recovery: the stuck clock runs out, a fresh worker and the reaper take over.
			e.backdate(t, jobID)
			e.start(t, q)
			e.startReaper(t, q)
			j := e.waitStatus(t, jobID, "done")
			waitEmpty(t, q)

			wantAttempts := 2
			if step == "committed" {
				wantAttempts = 1
			}
			if j.Attempts != wantAttempts {
				t.Fatalf("attempts = %d, want %d", j.Attempts, wantAttempts)
			}
			if st, ver := e.delivery(t, clipID); st != "hls" || ver != j.Version || ver == live {
				t.Fatalf("clip %s/%s, want the new %s", st, ver, j.Version)
			}
			if n := e.audits(t, jobID, "transcode_completed"); n != 1 {
				t.Fatalf("transcode_completed ×%d", n)
			}
			if n := e.count(t, `SELECT count(*) FROM renditions WHERE hls_version = $1`, j.Version); n != 2 { // 576p → 2 rungs
				t.Fatalf("live version has %d renditions, want 2", n)
			}
			if dead.Version != j.Version {
				if n := e.count(t, `SELECT count(*) FROM renditions WHERE hls_version = $1`, dead.Version); n != 0 {
					t.Fatalf("the dead attempt recorded %d renditions", n)
				}
				_, hasMaster := e.etags(t, prefix+"hls/"+dead.Version+"/")[prefix+"hls/"+dead.Version+"/master.m3u8"]
				if hasMaster != (step == "mastered") {
					t.Fatalf("dead attempt's orphan has master = %v", hasMaster)
				}
			}
			for v, objs := range everLive {
				if got := e.etags(t, prefix+"hls/"+v+"/"); !sameObjects(got, objs) {
					t.Fatalf("prefix %s was live and changed", v)
				}
			}
			live = j.Version
			everLive[live] = e.etags(t, prefix+"hls/"+live+"/")
		})
	}
}

// worker is a real cmd/worker process.
type worker struct {
	cmd *exec.Cmd
	out bytes.Buffer // read only after Wait
}

// isolatedQueue is the production queue namespace in Redis database 15, which nothing else
// uses: a `make worker` running on db 0 must never take a test's job, and the subprocess cannot
// be told another namespace (there is no such setting).
func (e *jobEnv) isolatedQueue(t *testing.T) (redisURL string, q Queue) {
	t.Helper()
	u, err := url.Parse(e.cfg.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/15"
	opt, err := redis.ParseURL(u.String())
	if err != nil {
		t.Fatal(err)
	}
	q = NewQueue(redis.NewClient(opt))
	q.R.Del(context.Background(), q.pending(), q.processing())
	t.Cleanup(func() {
		q.R.Del(context.Background(), q.pending(), q.processing())
		q.R.Close()
	})
	return u.String(), q
}

func buildWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "worker")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/worker").CombinedOutput(); err != nil {
		t.Fatalf("building the worker: %v\n%s", err, out)
	}
	return bin
}

// spawn starts the worker in its own process group (so a SIGKILL can take ffmpeg with it, like
// a container being killed) against the test database, test bucket and redisURL.
func (e *jobEnv) spawn(t *testing.T, bin, redisURL, workDir string) *worker {
	t.Helper()
	w := &worker{cmd: exec.Command(bin)}
	w.cmd.Env = append(os.Environ(),
		"DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"), "S3_BUCKET="+e.cfg.S3.Bucket, "REDIS_URL="+redisURL,
		"WORK_DIR="+workDir, "TRANSCODE_TIMEOUT=5m", "JOB_STUCK_AFTER=7m", "MAX_TRANSCODE_ATTEMPTS=3",
		"REQUEUE_AFTER=1s", "REAPER_INTERVAL=200ms", "SHUTDOWN_GRACE=0s")
	w.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	w.cmd.Stdout, w.cmd.Stderr = &w.out, &w.out
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if w.cmd.ProcessState == nil { // a failed test left it running
			syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
			w.cmd.Wait()
		}
		if t.Failed() {
			t.Logf("worker %d output:\n%s", w.cmd.Process.Pid, w.out.String())
		}
	})
	return w
}

// stop sends SIGTERM and waits for a clean exit.
func (w *worker) stop(t *testing.T) time.Duration {
	t.Helper()
	begin := time.Now()
	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker exited with %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("worker did not exit within a minute of SIGTERM")
	}
	return time.Since(begin)
}

// waitFFmpeg waits until an attempt has created its output dir — ffmpeg is about to run or
// running. EncodeRendition creates it right before starting ffmpeg.
func waitFFmpeg(t *testing.T, workDir string) {
	t.Helper()
	wait(t, 2*time.Minute, "ffmpeg to start", func() bool {
		m, _ := filepath.Glob(filepath.Join(workDir, "transcode-*", "hls", "*"))
		return len(m) > 0
	})
}

// §12 #31, the real one: SIGKILL the worker process (and its ffmpeg) mid-encode. Nothing is
// recorded; a restarted worker's own reaper calls the job stuck and the job is published on
// attempt 2.
func TestWorkerSIGKILL(t *testing.T) {
	e := setupJobs(t)
	redisURL, q := e.isolatedQueue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	if err := q.Push(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	bin, work := buildWorker(t), t.TempDir()

	w := e.spawn(t, bin, redisURL, work)
	waitFFmpeg(t, work)
	if err := syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := w.cmd.Wait(); !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("worker exit = %v, want killed", err)
	}
	dead := e.jobRow(t, jobID)
	if dead.Status != "processing" || dead.Attempts != 1 {
		t.Fatalf("after SIGKILL: job %+v, want processing (nothing recorded)", dead)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatalf("clip %s after a killed attempt", st)
	}

	e.backdate(t, jobID) // JOB_STUCK_AFTER has passed
	w2 := e.spawn(t, bin, redisURL, work)
	j := e.waitStatus(t, jobID, "done")
	waitEmpty(t, q)
	w2.stop(t)
	if j.Attempts != 2 || j.Version == dead.Version {
		t.Fatalf("job = %+v, want done on a new attempt 2", j)
	}
	if st, ver := e.delivery(t, clipID); st != "hls" || ver != j.Version {
		t.Fatalf("clip %s/%s, want hls/%s", st, ver, j.Version)
	}
	if n := e.count(t, `SELECT count(*) FROM renditions WHERE hls_version = $1`, dead.Version); n != 0 {
		t.Fatalf("the killed attempt recorded %d renditions", n)
	}
	if e.audits(t, jobID, "job_requeued") != 1 || e.audits(t, jobID, "transcode_completed") != 1 {
		t.Fatal("want one job_requeued and one transcode_completed")
	}
}

// §12 #37, the real one: SIGTERM with SHUTDOWN_GRACE=0 mid-encode. The process exits 0 promptly,
// ffmpeg is gone with it, the job is retryable with its attempt given back; a restarted worker
// re-pushes it through its reaper and finishes it on attempt 1.
func TestWorkerSIGTERM(t *testing.T) {
	e := setupJobs(t)
	redisURL, q := e.isolatedQueue(t)
	clipID, _ := e.clip(t, "clip_576p.mp4")
	jobID := e.job(t, clipID)
	if err := q.Push(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	bin, work := buildWorker(t), t.TempDir()

	w := e.spawn(t, bin, redisURL, work)
	waitFFmpeg(t, work)
	if d := w.stop(t); d > 15*time.Second {
		t.Fatalf("SIGTERM took %s with no grace", d)
	}
	if err := syscall.Kill(-w.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a process of the worker's group outlived it (ffmpeg not killed): %v", err)
	}
	j := e.jobRow(t, jobID)
	if j.Status != "retryable" || j.Reason != ReasonShutdown || j.Attempts != 0 {
		t.Fatalf("job = %+v, want retryable/shutdown with attempts 0", j)
	}
	if st, _ := e.delivery(t, clipID); st != "mp4" {
		t.Fatalf("clip %s after an interrupted attempt", st)
	}
	// Nothing stranded on the processing list. The queue itself may hold a duplicate: the
	// reaper's first sweep can re-push the row (older than REQUEUE_AFTER) in the instant after
	// the loop BLMOVEd it but before the claim — harmless, the claim lets one worker run it.
	if _, w := llen(t, q); w != 0 {
		t.Fatalf("%d processing entries after shutdown, want 0", w)
	}

	w2 := e.spawn(t, bin, redisURL, work)
	if j := e.waitStatus(t, jobID, "done"); j.Attempts != 1 {
		t.Fatalf("attempts = %d after the restart, want 1", j.Attempts)
	}
	w2.stop(t)
}
