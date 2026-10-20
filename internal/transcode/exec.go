// Package transcode invokes ffmpeg/ffprobe. Every invocation is bounded by a context deadline
// (exec.CommandContext): no unbounded child processes.
package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// stderrTail is how much ffmpeg stderr is kept: the end is where the actual error is.
const stderrTail = 8 << 10

// run executes name with args and returns stdout. On failure the error carries the tail of
// stderr — for ffmpeg that is the only useful diagnostic.
func run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%s: %w", name, ctx.Err()) // timeout or cancel, not a media problem
		}
		return stdout.Bytes(), Tail(stderr.Bytes()), fmt.Errorf("%s failed: %w: %s", name, err, Tail(stderr.Bytes()))
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// Tail returns at most the last 8 KiB of b.
func Tail(b []byte) []byte {
	if len(b) > stderrTail {
		return b[len(b)-stderrTail:]
	}
	return b
}
