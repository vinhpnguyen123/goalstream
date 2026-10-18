package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	probeTimeout  = 30 * time.Second
	decodeTimeout = 3 * time.Minute // a full decode of a 60 s clip; generous for slow machines
)

// Probe is what ffprobe tells us about a file. Bitrates are bits/sec; 0 means "not reported"
// (MKV containers report no per-stream bitrate — DATASET.md).
type Probe struct {
	HasVideo   bool
	HasAudio   bool
	Width      int
	Height     int
	VideoBps   int // first video stream's bit_rate
	FormatBps  int // whole-container bit_rate
	DurationMS int
}

func ProbeFile(ctx context.Context, path string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, _, err := run(ctx, "ffprobe", "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path)
	if err != nil {
		return Probe{}, fmt.Errorf("probing %s: %w", path, err)
	}
	p, err := parseProbe(out)
	if err != nil {
		return Probe{}, fmt.Errorf("probing %s: %w", path, err)
	}
	return p, nil
}

// DecodeCheck decodes the whole file and fails if ffmpeg reports anything at error level.
// A file can probe fine and still be broken mid-stream; this is the QC gate's last check.
func DecodeCheck(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, decodeTimeout)
	defer cancel()
	_, stderr, err := run(ctx, "ffmpeg", "-nostdin", "-v", "error", "-i", path, "-f", "null", "-")
	if err != nil {
		return err
	}
	if len(stderr) > 0 {
		return fmt.Errorf("decode errors: %s", Tail(stderr))
	}
	return nil
}

type ffprobeOutput struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		BitRate   string `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

func parseProbe(data []byte) (Probe, error) {
	var o ffprobeOutput
	if err := json.Unmarshal(data, &o); err != nil {
		return Probe{}, fmt.Errorf("parsing ffprobe json: %w", err)
	}
	var p Probe
	for _, s := range o.Streams {
		switch s.CodecType {
		case "video":
			if !p.HasVideo { // first video stream only — same choice as `-map 0:v:0`
				p.HasVideo, p.Width, p.Height, p.VideoBps = true, s.Width, s.Height, atoi(s.BitRate)
			}
		case "audio":
			p.HasAudio = true
		}
	}
	p.FormatBps = atoi(o.Format.BitRate)
	if d, err := strconv.ParseFloat(o.Format.Duration, 64); err == nil {
		p.DurationMS = int(math.Round(d * 1000))
	}
	return p, nil
}

// atoi treats absent or "N/A" values as 0 ("not reported").
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
