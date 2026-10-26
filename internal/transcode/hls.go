package transcode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// renditionTimeout bounds one rung even when the caller's context has no deadline. The worker's
// TRANSCODE_TIMEOUT around the whole attempt is the real budget; this is the backstop.
const renditionTimeout = 10 * time.Minute

// Rendition is a rung as actually produced.
type Rendition struct {
	Rung
	Width    int // read back from the output: scale=-2 rounding is ffmpeg's, not ours
	Segments int
}

// Dir is the rendition's directory under the version prefix ("720p").
func (r Rendition) Dir() string { return strconv.Itoa(r.Height) + "p" }

// EncodeRendition encodes src into <outDir>/<h>p/index.m3u8 + seg_00000.ts … for one rung.
//
// The flags that are not optional on any rendition (CONVENTIONS.md § ffmpeg):
//   - keyframes forced every 2 s by TIME, so with 4 s segments every rung cuts at the same
//     instants and the player can switch mid-stream cleanly (-g 48 would be 1.92 s at 25 fps);
//   - -maxrate/-bufsize, so the encoder's peak is bounded and BANDWIDTH is a true upper bound;
//   - yuv420p, High@4.0 for browser decoders; scale=-2 keeps the aspect with an even width.
//
// Audio is mapped without `?`: the worker probes first and fails no_audio_stream before here.
func EncodeRendition(ctx context.Context, src, outDir string, r Rung) (Rendition, error) {
	ctx, cancel := context.WithTimeout(ctx, renditionTimeout)
	defer cancel()
	rd := Rendition{Rung: r}
	dir := filepath.Join(outDir, rd.Dir())
	if err := os.MkdirAll(dir, 0o755); err != nil { // the hls muxer does not create it
		return rd, fmt.Errorf("encoding %s: %w", rd.Dir(), err)
	}
	_, _, err := run(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", src,
		"-map", "0:v:0", "-map", "0:a:0",
		"-vf", "scale=-2:"+strconv.Itoa(r.Height),
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-level", "4.0",
		"-b:v", strconv.Itoa(r.VideoBps), "-maxrate", strconv.Itoa(r.MaxRate), "-bufsize", strconv.Itoa(r.BufSize),
		"-force_key_frames", "expr:gte(t,n_forced*2)", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", strconv.Itoa(r.AudioBps), "-ac", "2", "-ar", "48000",
		"-f", "hls", "-hls_time", "4", "-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(dir, "seg_%05d.ts"),
		filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return rd, fmt.Errorf("encoding %s: %w", rd.Dir(), err)
	}
	segs, err := filepath.Glob(filepath.Join(dir, "seg_*.ts"))
	if err != nil || len(segs) == 0 {
		return rd, fmt.Errorf("encoding %s: no segments written", rd.Dir())
	}
	p, err := ProbeFile(ctx, segs[0])
	if err != nil || p.Width == 0 {
		return rd, fmt.Errorf("reading back %s width: %v", rd.Dir(), err)
	}
	rd.Width, rd.Segments = p.Width, len(segs)
	return rd, nil
}

// Thumbnail writes one 360p JPEG frame from 25% into src: past any lead-in, and for a goal clip
// (15 s pre-roll of 60) just before the goal.
func Thumbnail(ctx context.Context, src, dst string, durMS int) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, _, err := run(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-ss", msToSeconds(durMS/4), "-i", src, "-frames:v", "1", "-vf", "scale=-2:360", "-q:v", "3", dst)
	if err != nil {
		return fmt.Errorf("thumbnail: %w", err)
	}
	return nil
}

// codecs matches -profile:v high -level 4.0 (avc1.64 0028) and AAC-LC (mp4a.40.2).
const codecs = `avc1.640028,mp4a.40.2`

// MasterPlaylist lists the renditions, tallest first. BANDWIDTH is the PEAK (maxrate + audio +
// container), as the HLS spec requires — an average there makes players pick a rung the link
// cannot sustain. AVERAGE-BANDWIDTH carries the target.
func MasterPlaylist(rds []Rendition) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, r := range rds {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=%q\n%s/index.m3u8\n",
			r.Bandwidth, r.VideoBps+r.AudioBps, r.Width, r.Height, codecs, r.Dir())
	}
	return []byte(b.String())
}
