package transcode

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// CutMezzanine re-encodes [startMS, startMS+durMS) of src into an MP4 mezzanine at dst.
//
// Re-encode, never stream-copy: `-c copy` snaps the cut to the nearest keyframe, which on
// broadcast footage can be seconds away — enough to miss the goal. capBps is the -maxrate
// (decision D15: never above the source's own bitrate).
//
// Explicit -map: first video, first audio IF ANY (`?`). Never choosing a track by language
// keeps the code free of hardcoded languages; mapping explicitly also drops the ASS subtitle
// stream SoccerNet files carry. With no audio the output is video-only, and the QC gate then
// rejects it with the precise reason instead of a generic ffmpeg failure.
func CutMezzanine(ctx context.Context, src, dst string, startMS, durMS, crf, capBps int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _, err := run(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-ss", msToSeconds(startMS), "-i", src, "-t", msToSeconds(durMS),
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "slow", "-crf", strconv.Itoa(crf),
		"-maxrate", strconv.Itoa(capBps), "-bufsize", strconv.Itoa(2*capBps),
		"-pix_fmt", "yuv420p", "-force_key_frames", "expr:gte(t,n_forced*2)",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000",
		"-movflags", "+faststart",
		dst)
	if err != nil {
		return fmt.Errorf("cutting %d+%d ms: %w", startMS, durMS, err)
	}
	return nil
}

// msToSeconds formats milliseconds as an exact decimal seconds string ("1641.477").
func msToSeconds(ms int) string {
	return fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
}
